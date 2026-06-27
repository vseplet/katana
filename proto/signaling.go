package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"

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
}

// scrollMsg — событие прокрутки от браузера (в «кликах» колеса).
type scrollMsg struct {
	Dx int `json:"dx"`
	Dy int `json:"dy"`
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

// optsFromQuery строит опции захвата для соединения: базовые (флаги сервера),
// переопределённые query-параметрами при коннекте. Кодек задаётся только так
// (на лету не меняется — нужен новый трек), остальное можно менять и через
// config-сообщения уже в сессии.
func optsFromQuery(base capture.Options, q url.Values) capture.Options {
	var c configMsg
	if q.Has("sourceKind") {
		v := q.Get("sourceKind")
		c.SourceKind = &v
	}
	if v, ok := queryInt(q, "sourceId"); ok {
		c.SourceID = &v
	}
	if v, ok := queryInt(q, "screen"); ok {
		c.Screen = &v
	}
	if v, ok := queryInt(q, "width"); ok {
		c.Width = &v
	}
	if v, ok := queryInt(q, "fps"); ok {
		c.FPS = &v
	}
	if v, ok := queryInt(q, "bitrateKbps"); ok {
		c.BitrateKbps = &v
	}
	if v, ok := queryInt(q, "threads"); ok {
		c.Threads = &v
	}
	if q.Has("dropLate") {
		b := q.Get("dropLate") == "true"
		c.DropLate = &b
	}
	if q.Has("cursor") {
		b := q.Get("cursor") == "true"
		c.Cursor = &b
	}
	o := c.apply(base)
	switch q.Get("codec") {
	case "h264":
		o.Codec = capture.CodecH264
	case "vp8":
		o.Codec = capture.CodecVP8
	}
	// Звук — только при коннекте (добавление дорожки = ренеготиация).
	o.Audio = q.Get("audio") == "true"
	return o
}

func queryInt(q url.Values, key string) (int, bool) {
	if !q.Has(key) {
		return 0, false
	}
	v, err := strconv.Atoi(q.Get(key))
	if err != nil {
		return 0, false
	}
	return v, true
}

// signalingHandler возвращает http.HandlerFunc для /ws.
//
// На каждое WS-соединение поднимается отдельный PeerConnection с захватом.
// Go — инициатор оффера, браузер отвечает (см. §4 ТЗ).
//
// root — корневой контекст сервера: его отмена (SIGINT/SIGTERM) останавливает
// захваты всех активных зрителей.
func signalingHandler(root context.Context, enc capture.CaptureEncoder, opts capture.Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// localhost-прототип: не привередничаем к Origin.
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("signaling: ws accept: %v", err)
			return
		}
		connOpts := optsFromQuery(opts, r.URL.Query())
		log.Printf("signaling: viewer connected (%s) codec=%s", r.RemoteAddr, connOpts.Codec)

		// Контекст соединения наследуется от корневого: отменяется и при
		// отключении зрителя (defer cancel), и при остановке сервера (root).
		ctx, cancel := context.WithCancel(root)
		defer cancel()

		s := &session{conn: conn, base: connOpts}
		s.setSource(connOpts.SourceKind, connOpts.SourceID) // геометрия для мыши
		defer func() {
			_ = conn.CloseNow()
			log.Printf("signaling: viewer disconnected (%s)", r.RemoteAddr)
		}()

		pc, str, err := newPeerConnection(ctx, enc, connOpts)
		if err != nil {
			log.Printf("signaling: peer connection: %v", err)
			return
		}
		defer func() {
			str.stop()
			if err := pc.Close(); err != nil {
				log.Printf("signaling: pc close: %v", err)
			}
		}()
		s.pc = pc
		s.str = str

		// Трикл ICE: каждый локальный кандидат уходит зрителю.
		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				return // сбор кандидатов завершён
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
				cancel()
			}
		})

		// Канал ввода (ordered+reliable) поверх того же peer-соединения — низкая
		// задержка, единый транспорт. Go создаёт его до оффера; браузер шлёт сюда
		// mouse/scroll/cursor. Создаём ДО CreateOffer, чтобы попал в SDP.
		if dc, err := pc.CreateDataChannel("input", nil); err != nil {
			log.Printf("signaling: data channel: %v", err)
		} else {
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				var im signalMessage
				if json.Unmarshal(m.Data, &im) != nil {
					return
				}
				s.dispatchInput(&im)
			})
		}

		// Go офферит первым.
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			log.Printf("signaling: create offer: %v", err)
			return
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			log.Printf("signaling: set local description: %v", err)
			return
		}
		s.send(ctx, signalMessage{Type: "offer", SDP: offer.SDP})

		// Цикл чтения: answer + кандидаты от браузера.
		readLoop(ctx, s)
	}
}

// session держит WS-соединение, PeerConnection и streamer одного зрителя.
type session struct {
	conn *websocket.Conn
	pc   *webrtc.PeerConnection
	str  *streamer
	base capture.Options // базовые опции (индекс экрана, дефолты)
	mu   sync.Mutex      // сериализует запись в WS (OnICECandidate + readLoop)

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
			// Нормальное закрытие или отмена контекста.
			if ctx.Err() == nil && websocket.CloseStatus(err) == -1 && !errors.Is(err, context.Canceled) {
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
		case "answer":
			ans := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP}
			if err := s.pc.SetRemoteDescription(ans); err != nil {
				log.Printf("signaling: set remote description: %v", err)
			}
		case "candidate":
			if msg.Candidate == nil {
				continue
			}
			if err := s.pc.AddICECandidate(*msg.Candidate); err != nil {
				log.Printf("signaling: add ice candidate: %v", err)
			}
		case "config":
			if msg.Config == nil {
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
		case "mouse", "scroll", "cursor":
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
		if msg.Config != nil && msg.Config.Cursor != nil {
			s.str.updateCursor(*msg.Config.Cursor)
		}
	}
}
