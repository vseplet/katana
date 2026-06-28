package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/vseplet/katana/proto/capture"
)

// signalMessage — формат сообщений сигналинга (JSON over WS), см. §4 ТЗ.
// Типы "config"/"mouse" — расширения поверх ТЗ.
type signalMessage struct {
	Type      string                   `json:"type"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Config    *configMsg               `json:"config,omitempty"`
	Mouse     *mouseMsg                `json:"mouse,omitempty"`
	Scroll    *scrollMsg               `json:"scroll,omitempty"`
	Key       *keyMsg                  `json:"key,omitempty"`
	Text      string                   `json:"text,omitempty"` // для "type": набор текста
	// Параметры для "hello" — зритель выбирает кодек и звук при подключении
	// (как кодек/аудио меняются: новый трек = нужен новый PeerConnection).
	Codec string `json:"codec,omitempty"`
	Audio *bool  `json:"audio,omitempty"`
	// Источники захвата: запрос ("sources") и ответ хоста (Sources); активация
	// приложения ("activate" + PID). Раньше это было HTTP-API хоста.
	Sources *capture.Sources `json:"sources,omitempty"`
	PID     int              `json:"pid,omitempty"`
}

// scrollMsg — событие прокрутки от браузера (в «кликах» колеса).
type scrollMsg struct {
	Dx int `json:"dx"`
	Dy int `json:"dy"`
}

// keyMsg — нажатие клавиши с модификаторами (спец-клавиши и шорткаты).
type keyMsg struct {
	Key  string   `json:"key"`
	Mods []string `json:"mods,omitempty"`
}

// mouseMsg — событие мыши от браузера. X/Y — нормализованные [0,1] координаты
// относительно содержимого видео.
type mouseMsg struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Action string  `json:"action"` // move | down | up
	Button string  `json:"button"` // left | right
}

// configMsg — настройки захвата, присылаемые браузером. Указатели, чтобы
// отличать «не задано» от нуля; незаданные поля сохраняют текущее значение.
type configMsg struct {
	SourceKind  *string `json:"sourceKind,omitempty"` // screen | window | app
	SourceID    *int    `json:"sourceId,omitempty"`   // windowID / pid (для window/app)
	Screen      *int    `json:"screen,omitempty"`     // индекс avfoundation (для screen)
	Width       *int    `json:"width,omitempty"`
	FPS         *int    `json:"fps,omitempty"`
	BitrateKbps *int    `json:"bitrateKbps,omitempty"`
	Threads     *int    `json:"threads,omitempty"`
	DropLate    *bool   `json:"dropLate,omitempty"`
	Cursor      *bool   `json:"cursor,omitempty"`
}

// apply накладывает настройки на базовые опции с клампингом разумных границ.
// Битрейт приходит числом (kbps) — не строкой, чтобы не пробрасывать
// произвольный текст в аргументы ffmpeg.
func (c *configMsg) apply(base capture.Options) capture.Options {
	o := base
	if c.SourceKind != nil {
		o.SourceKind = *c.SourceKind
	}
	if c.SourceID != nil {
		o.SourceID = *c.SourceID
	}
	if c.Screen != nil {
		o.ScreenIndex = clamp(*c.Screen, 0, 64)
	}
	if c.Width != nil {
		if *c.Width == 0 {
			o.Width = 0 // нативное разрешение (без даунскейла)
		} else {
			o.Width = clamp(*c.Width, 320, 7680)
		}
	}
	if c.FPS != nil {
		o.FPS = clamp(*c.FPS, 1, 60)
	}
	if c.BitrateKbps != nil {
		o.Bitrate = fmt.Sprintf("%dk", clamp(*c.BitrateKbps, 100, 20000))
	}
	if c.Threads != nil {
		o.Threads = clamp(*c.Threads, 0, 16)
	}
	if c.DropLate != nil {
		o.DropLate = *c.DropLate
	}
	if c.Cursor != nil {
		o.Cursor = *c.Cursor
	}
	return o
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// runBrokerHost подключается исходящим WS к рандеву-брокеру как host и ведёт
// сессию через него (режим: katana --id=<uuid>). После ухода зрителя
// переподключается, чтобы ждать следующего.
func runBrokerHost(ctx context.Context, brokerURL, sessionID string, enc capture.CaptureEncoder, opts capture.Options) {
	wsURL := fmt.Sprintf("%s?session=%s&role=host",
		strings.TrimRight(brokerURL, "/"), url.QueryEscape(sessionID))
	for ctx.Err() == nil {
		log.Printf("broker: подключаюсь к %s (session %s)", brokerURL, sessionID)
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			log.Printf("broker: dial: %v (повтор через 3с)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		log.Printf("broker: подключён, жду зрителя")
		serveSession(ctx, conn, enc, opts, "broker:"+sessionID)
		if ctx.Err() == nil {
			log.Printf("broker: сессия завершена, переподключаюсь")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// serveSession ведёт одну сессию поверх готового WS-соединения. Host —
// инициатор оффера; оффер шлётся в ответ на "hello" от зрителя (в broker-режиме
// хост подключается раньше зрителя, поэтому сразу слать оффер нельзя).
func serveSession(parent context.Context, conn *websocket.Conn, enc capture.CaptureEncoder, connOpts capture.Options, label string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	s := &session{conn: conn, enc: enc, cancel: cancel, base: connOpts}
	defer func() {
		if s.str != nil {
			s.str.stop()
		}
		if s.pc != nil {
			if err := s.pc.Close(); err != nil {
				log.Printf("signaling: pc close: %v", err)
			}
		}
		_ = conn.CloseNow()
		log.Printf("signaling: session ended (%s)", label)
	}()

	// PeerConnection и захват создаются лениво по "hello" (с кодеком/аудио зрителя).
	readLoop(ctx, s)
}

// buildOpts накладывает выбор зрителя (config + codec + audio) на базовые опции.
func (s *session) buildOpts(msg signalMessage) capture.Options {
	opts := s.base
	if msg.Config != nil {
		opts = msg.Config.apply(s.base)
	}
	switch msg.Codec {
	case "h264":
		opts.Codec = capture.CodecH264
	case "vp8":
		opts.Codec = capture.CodecVP8
	}
	if msg.Audio != nil {
		opts.Audio = *msg.Audio
	}
	return opts
}

// offerStream стартует захват и шлёт offer зрителю. true при успехе.
func (s *session) offerStream(ctx context.Context, opts capture.Options) bool {
	if err := s.startStream(ctx, opts); err != nil {
		log.Printf("signaling: start stream: %v", err)
		return false
	}
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("signaling: create offer: %v", err)
		return false
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		log.Printf("signaling: set local description: %v", err)
		return false
	}
	s.send(ctx, signalMessage{Type: "offer", SDP: offer.SDP})
	return true
}

// renegotiateTracks меняет треки на УЖЕ установленном pc и шлёт оффер
// ренеготиации — без пересоздания соединения. Видео-трек пересоздаём всегда
// (новый SSRC → Chrome заводит свежий декодер: H264 не переживает смену
// параметров на лету по интернету; заодно меняется кодек). Opus-дорожку
// добавляем/снимаем по opts.Audio. Выполняется в readLoop (последовательно).
func (s *session) renegotiateTracks(ctx context.Context, opts capture.Options) {
	s.str.stop() // остановить захват перед подменой треков

	if s.videoSender != nil {
		_ = s.pc.RemoveTrack(s.videoSender)
	}
	mime := webrtc.MimeTypeVP8
	if opts.Codec == capture.CodecH264 {
		mime = webrtc.MimeTypeH264
	}
	vtrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: mime}, "screen", "desktop")
	if err != nil {
		log.Printf("reneg: new video track: %v", err)
		s.cancel()
		return
	}
	vsender, err := s.pc.AddTrack(vtrack)
	if err != nil {
		log.Printf("reneg: add video track: %v", err)
		s.cancel()
		return
	}
	go readRTCP(vsender)
	s.videoSender = vsender

	var atrack *webrtc.TrackLocalStaticSample
	if opts.Audio {
		atrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "audio")
		if err != nil {
			log.Printf("reneg: new audio track: %v", err)
			s.cancel()
			return
		}
		asender, err := s.pc.AddTrack(atrack)
		if err != nil {
			log.Printf("reneg: add audio track: %v", err)
			s.cancel()
			return
		}
		go readRTCP(asender)
		s.audioSender = asender
	} else if s.audioSender != nil {
		_ = s.pc.RemoveTrack(s.audioSender)
		s.audioSender = nil
	}
	s.str.setTracks(vtrack, atrack)

	// Оффер ренеготиации — сразу после смены треков (медиа польётся, когда
	// захват перезапустится ниже; SDP описывает треки, а не кадры).
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("reneg: create offer: %v", err)
		s.cancel()
		return
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		log.Printf("reneg: set local description: %v", err)
		s.cancel()
		return
	}
	s.send(ctx, signalMessage{Type: "offer", SDP: offer.SDP})
	log.Printf("broker: ренеготиация (codec=%s audio=%v) — без разрыва", opts.Codec, opts.Audio)

	// Рестарт захвата под новые треки — в фоне (~1с), чтобы не блокировать readLoop.
	go func() {
		if err := s.str.reconfigure(opts); err != nil {
			log.Printf("reneg: reconfigure: %v", err)
		}
		s.setSource(opts.SourceKind, opts.SourceID)
	}()
}

// startStream строит PeerConnection с выбранными зрителем кодеком/аудио, вешает
// каналы данных (input/term) и стартует захват. Вызывается из "hello".
func (s *session) startStream(ctx context.Context, opts capture.Options) error {
	pc, str, err := newPeerConnection(ctx, s.enc, opts)
	if err != nil {
		return err
	}
	s.pc = pc
	s.str = str
	// Запоминаем сендеры — для замены треков при ренеготиации (без нового pc).
	s.videoSender, s.audioSender = nil, nil
	for _, snd := range pc.GetSenders() {
		if snd.Track() == nil {
			continue
		}
		switch snd.Track().Kind() {
		case webrtc.RTPCodecTypeVideo:
			s.videoSender = snd
		case webrtc.RTPCodecTypeAudio:
			s.audioSender = snd
		}
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		s.send(ctx, signalMessage{Type: "candidate", Candidate: &init})
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("signaling: connection state -> %s", state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.cancel()
		}
	})

	// Каналы данных создаём ДО оффера → попадут в SDP.
	if dc, err := pc.CreateDataChannel("input", nil); err != nil {
		log.Printf("signaling: data channel: %v", err)
	} else {
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			var im signalMessage
			if json.Unmarshal(m.Data, &im) != nil {
				return
			}
			switch im.Type {
			case "sources":
				// Список источников захвата — ответ по тому же каналу (P2P).
				if src, err := capture.ListSources(); err == nil {
					if b, err := json.Marshal(signalMessage{Type: "sources", Sources: &src}); err == nil {
						_ = dc.SendText(string(b))
					}
				}
			case "activate":
				if im.PID > 0 {
					_ = capture.ActivateApp(im.PID)
				}
			default:
				s.dispatchInput(&im)
			}
		})
	}
	if dc, err := pc.CreateDataChannel("term", nil); err != nil {
		log.Printf("signaling: term channel: %v", err)
	} else {
		sharedTerminal.bind(dc)
	}

	if err := str.reconfigure(opts); err != nil {
		return err
	}
	s.setSource(opts.SourceKind, opts.SourceID)
	return nil
}

// session держит WS-соединение, PeerConnection и streamer одного зрителя.
// pc/str создаются лениво по "hello" (с кодеком/аудио зрителя).
type session struct {
	conn   *websocket.Conn
	enc    capture.CaptureEncoder
	cancel context.CancelFunc // отменяет контекст сессии (по разрыву PC)
	pc          *webrtc.PeerConnection
	str         *streamer
	videoSender *webrtc.RTPSender // для замены видео-трека при ренеготиации
	audioSender *webrtc.RTPSender // для добавления/снятия Opus-дорожки
	base        capture.Options   // базовые опции (источник/экран/дефолты)
	mu          sync.Mutex        // сериализует запись в WS (OnICECandidate + readLoop)

	srcMu sync.Mutex   // защищает геометрию источника
	rect  capture.Rect // глобальный прямоугольник источника (для маппинга мыши)

	btnDown string // зажатая кнопка мыши ("" если нет) — для drag; только из readLoop
	dragged bool   // были ли move с зажатой кнопкой (отличить drag от чистого клика)
}

// setSource обновляет кэш геометрии источника (для координат мыши). Вызов SCK
// относительно дорогой, поэтому делаем его при смене источника, а не на каждое
// событие мыши. window может двигаться — тогда геометрия слегка устаревает.
func (s *session) setSource(kind string, id int) {
	r, err := capture.SourceRect(kind, id)
	if err != nil {
		return // не SCK-источник или не найден — мышь просто не сработает
	}
	s.srcMu.Lock()
	s.rect = r
	s.srcMu.Unlock()
}

// handleMouse мапит нормализованные координаты в глобальные и инжектит событие.
func (s *session) handleMouse(m *mouseMsg) {
	s.srcMu.Lock()
	r := s.rect
	s.srcMu.Unlock()
	if r.W <= 0 || r.H <= 0 {
		return
	}
	x := int(r.X + clampF(m.X)*r.W)
	y := int(r.Y + clampF(m.Y)*r.H)
	button := "left"
	if m.Button == "right" {
		button = "right"
	}
	switch m.Action {
	case "down":
		moveMouse(x, y)
		mouseToggle(button, true)
		s.btnDown = button
		s.dragged = false
	case "up":
		// dragMouse шлём ТОЛЬКО если реально был drag (приходили move). Иначе
		// (чистый клик) — просто отпускаем: без события Dragged, чтобы тап не
		// принимался за перетаскивание/выделение.
		if s.btnDown != "" && s.dragged {
			dragMouse(x, y, s.btnDown)
		}
		mouseToggle(button, false)
		s.btnDown = ""
		s.dragged = false
	default: // move
		if s.btnDown != "" {
			dragMouse(x, y, s.btnDown) // зажата кнопка → drag-событие
			s.dragged = true
		} else {
			moveMouse(x, y)
		}
	}
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// send сериализует сообщение и пишет его в WS под мьютексом.
func (s *session) send(ctx context.Context, msg signalMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("signaling: marshal %s: %v", msg.Type, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.Write(ctx, websocket.MessageText, data); err != nil {
		if ctx.Err() == nil {
			log.Printf("signaling: write %s: %v", msg.Type, err)
		}
	}
}

// readLoop читает сообщения от браузера до закрытия соединения.
func readLoop(ctx context.Context, s *session) {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			// 1008 — брокер закрыл: сессия неизвестна/не авторизована.
			if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
				log.Printf("broker: сессия не найдена или не авторизована — проверь --session (полный UUID) и что она создана на том же сайте")
			} else if ctx.Err() == nil && websocket.CloseStatus(err) == -1 && !errors.Is(err, context.Canceled) {
				log.Printf("signaling: ws read: %v", err)
			}
			return
		}

		var msg signalMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("signaling: unmarshal: %v", err)
			continue
		}

		switch msg.Type {
		case "hello":
			// Зритель повторяет hello каждые ~1.5с, пока не получит оффер. Если
			// PC уже есть:
			//  - connected/failed/disconnected → реальное переподключение зрителя
			//    (reload) → пересоздаём сессию;
			//  - new/connecting → это просто дубль hello во время рукопожатия
			//    (старт захвата ~секунда) → игнорируем, иначе порвём собственную
			//    только что созданную сессию (петля «жду зрителя»).
			if s.pc != nil {
				switch s.pc.ConnectionState() {
				case webrtc.PeerConnectionStateConnected,
					webrtc.PeerConnectionStateFailed,
					webrtc.PeerConnectionStateDisconnected:
					log.Printf("broker: повторный hello (%s) — пересоздаю сессию", s.pc.ConnectionState())
					return
				default:
					continue // рукопожатие идёт — игнорируем дубль hello
				}
			}
			if !s.offerStream(ctx, s.buildOpts(msg)) {
				return
			}
		case "renegotiate":
			// Смена codec/audio/(H264-настроек) БЕЗ разрыва соединения: на ТОМ ЖЕ
			// pc подменяем видео-трек (новый SSRC → Chrome поднимает свежий
			// декодер, H264 иначе виснет) и добавляем/снимаем Opus-дорожку, затем
			// шлём оффер ренеготиации. pc не закрывается → нет гонок/рандеву.
			if s.pc == nil || s.str == nil {
				continue // нет активного потока — ждём hello
			}
			s.renegotiateTracks(ctx, s.buildOpts(msg))
		case "answer":
			if s.pc == nil {
				continue // ещё не было hello
			}
			ans := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP}
			if err := s.pc.SetRemoteDescription(ans); err != nil {
				log.Printf("signaling: set remote description: %v", err)
			}
		case "candidate":
			if s.pc == nil || msg.Candidate == nil {
				continue
			}
			if err := s.pc.AddICECandidate(*msg.Candidate); err != nil {
				log.Printf("signaling: add ice candidate: %v", err)
			}
		case "config":
			if msg.Config == nil || s.str == nil {
				continue
			}
			// Перезапуск ffmpeg может занять ~секунду — делаем в отдельной
			// горутине, чтобы не блокировать чтение WS (и приём ICE).
			newOpts := msg.Config.apply(s.base)
			go func() {
				if err := s.str.reconfigure(newOpts); err != nil {
					log.Printf("signaling: reconfigure: %v", err)
				}
				s.setSource(newOpts.SourceKind, newOpts.SourceID) // обновить геометрию
			}()
		case "mouse", "scroll", "cursor", "key", "type":
			s.dispatchInput(&msg) // фолбэк, если DataChannel ещё не открыт
		default:
			log.Printf("signaling: unknown message type %q", msg.Type)
		}
	}
}

// dispatchInput обрабатывает события ввода (mouse/scroll/cursor) — общий путь
// для DataChannel (основной) и WebSocket (фолбэк).
func (s *session) dispatchInput(msg *signalMessage) {
	switch msg.Type {
	case "mouse":
		if msg.Mouse != nil {
			s.handleMouse(msg.Mouse)
		}
	case "scroll":
		if msg.Scroll != nil {
			scrollMouse(msg.Scroll.Dx, msg.Scroll.Dy)
		}
	case "cursor":
		if s.str != nil && msg.Config != nil && msg.Config.Cursor != nil {
			s.str.updateCursor(*msg.Config.Cursor)
		}
	case "key":
		if msg.Key != nil && msg.Key.Key != "" {
			tapKey(msg.Key.Key, msg.Key.Mods)
		}
	case "type":
		if msg.Text != "" {
			typeText(msg.Text)
		}
	}
}
