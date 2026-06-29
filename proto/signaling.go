package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"runtime"
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
	// Vid — идентификатор зрителя (viewer/peer id). Несколько зрителей делят один
	// WS хоста через брокер; vid адресует сигналинг конкретному зрителю. Зритель
	// генерирует его при подключении и проставляет во все свои сообщения; хост
	// возвращает тот же vid в offer/candidate/state, чтобы зритель отфильтровал
	// чужие. (Отдельно от app-PID ниже — у activate своё числовое поле "pid".)
	Vid string `json:"vid,omitempty"`
	// Параметры для "hello" — зритель выбирает кодек и звук при подключении
	// (как кодек/аудио меняются: новый трек = нужен новый PeerConnection).
	Codec string `json:"codec,omitempty"`
	Audio *bool  `json:"audio,omitempty"`
	// Источники захвата: запрос ("sources") и ответ хоста (Sources); активация
	// приложения ("activate" + PID). Раньше это было HTTP-API хоста.
	Sources *capture.Sources `json:"sources,omitempty"`
	PID     int              `json:"pid,omitempty"`
	// Инфо о хосте (для "hostinfo" — заголовок вкладки зрителя).
	OS       string `json:"os,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// osLabel — человекочитаемое имя ОС хоста.
func osLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
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

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// optsToState строит "state"-сообщение с ТЕКУЩИМИ настройками трансляции для
// зрителя — чтобы его UI синхронизировался с уже идущим потоком, а не показывал
// собственные дефолты (и не отправлял их обратно).
func optsToState(o capture.Options) signalMessage {
	codec := "vp8"
	if o.Codec == capture.CodecH264 {
		codec = "h264"
	}
	audio := o.Audio
	kind := o.SourceKind
	sid := o.SourceID
	screen := o.ScreenIndex
	width := o.Width
	fps := o.FPS
	cursor := o.Cursor
	return signalMessage{
		Type:  "state",
		Codec: codec,
		Audio: &audio,
		Config: &configMsg{
			SourceKind: &kind,
			SourceID:   &sid,
			Screen:     &screen,
			Width:      &width,
			FPS:        &fps,
			Cursor:     &cursor,
		},
	}
}

// runBrokerHost подключается исходящим WS к рандеву-брокеру как host и ведёт
// сессию через него (режим: katana --id=<uuid>). WS живёт постоянно, пока хост
// запущен; несколько зрителей подключаются и отключаются через него.
func runBrokerHost(ctx context.Context, brokerURL, sessionID string, enc capture.CaptureEncoder, opts capture.Options) {
	wsURL := fmt.Sprintf("%s?session=%s&role=host",
		strings.TrimRight(brokerURL, "/"), url.QueryEscape(sessionID))
	for ctx.Err() == nil {
		log.Printf("broker: connecting to %s (session %s)", brokerURL, sessionID)
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			log.Printf("broker: dial: %v (retry in 3s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		log.Printf("broker: connected, waiting for viewers")
		serveHub(ctx, conn, enc, opts, "broker:"+sessionID)
		if ctx.Err() == nil {
			log.Printf("broker: connection lost, reconnecting")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// hub — хост-узел: одно WS-соединение с брокером, ОДИН общий захват и набор
// зрителей (peer). Видео/аудио-треки общие: pion раздаёт WriteSample во все
// привязанные PeerConnection, поэтому экран кодируется один раз на всех.
//
// Настройки трансляции (кодек/источник/разрешение/аудио) — «липкие»: их задаёт
// первый зритель, дальше они живут в hub и применяются ко всем; новый зритель
// синхронизируется с ними, а не навязывает свои.
type hub struct {
	ctx  context.Context
	enc  capture.CaptureEncoder
	base capture.Options
	ws   *websocket.Conn
	cnl  context.CancelFunc

	writeMu sync.Mutex // сериализует запись в общий WS

	mu         sync.Mutex // защищает всё ниже
	configured bool       // задавались ли уже настройки трансляции
	curOpts    capture.Options
	str        *streamer
	vtrack     *webrtc.TrackLocalStaticSample
	atrack     *webrtc.TrackLocalStaticSample
	peers      map[string]*peer

	srcMu sync.Mutex   // защищает геометрию источника (для координат мыши)
	rect  capture.Rect // глобальный прямоугольник общего источника
}

// peer — один зритель: своё PeerConnection поверх общих треков хаба и свои
// data-каналы (input/term). Видео/аудио-треки НЕ свои — общие из hub.
type peer struct {
	h           *hub
	pid         string
	pc          *webrtc.PeerConnection
	videoSender *webrtc.RTPSender
	audioSender *webrtc.RTPSender

	btnDown string // зажатая кнопка мыши ("" если нет) — для drag
	dragged bool   // были ли move с зажатой кнопкой (отличить drag от клика)
}

// serveHub ведёт хост-узел поверх готового WS-соединения с брокером.
func serveHub(parent context.Context, conn *websocket.Conn, enc capture.CaptureEncoder, connOpts capture.Options, label string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	h := &hub{
		ctx:   ctx,
		enc:   enc,
		base:  connOpts,
		ws:    conn,
		cnl:   cancel,
		peers: map[string]*peer{},
	}
	defer func() {
		h.mu.Lock()
		for pid, p := range h.peers {
			p.closePC()
			delete(h.peers, pid)
		}
		if h.str != nil {
			h.str.stop()
			h.str = nil
		}
		h.mu.Unlock()
		_ = conn.CloseNow()
		log.Printf("signaling: host session ended (%s)", label)
	}()

	h.readLoop()
}

// send сериализует сообщение (с проставленным pid) и пишет его в общий WS.
func (h *hub) send(msg signalMessage, pid string) {
	msg.Vid = pid
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("signaling: marshal %s: %v", msg.Type, err)
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if err := h.ws.Write(h.ctx, websocket.MessageText, data); err != nil {
		if h.ctx.Err() == nil {
			log.Printf("signaling: write %s: %v", msg.Type, err)
		}
	}
}

// readLoop читает сигналинг всех зрителей с общего WS и роутит по pid.
func (h *hub) readLoop() {
	for {
		_, data, err := h.ws.Read(h.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
				log.Printf("broker: session not found or unauthorized — check --session (full UUID) and that it was created on the same site")
			} else if h.ctx.Err() == nil && websocket.CloseStatus(err) == -1 && !errors.Is(err, context.Canceled) {
				log.Printf("signaling: ws read: %v", err)
			}
			return
		}

		var msg signalMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("signaling: unmarshal: %v", err)
			continue
		}
		pid := msg.Vid
		if pid == "" {
			pid = "default" // зритель без vid (старый клиент) — единственный «default»
		}

		switch msg.Type {
		case "hello":
			h.onHello(pid, msg)
		case "renegotiate":
			h.onRenegotiate(msg)
		case "answer":
			if p := h.peer(pid); p != nil {
				ans := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP}
				if err := p.pc.SetRemoteDescription(ans); err != nil {
					log.Printf("signaling: set remote description: %v", err)
				}
			}
		case "candidate":
			if p := h.peer(pid); p != nil && msg.Candidate != nil {
				if err := p.pc.AddICECandidate(*msg.Candidate); err != nil {
					log.Printf("signaling: add ice candidate: %v", err)
				}
			}
		case "config":
			h.onConfig(msg)
		case "mouse", "scroll", "cursor", "key", "type":
			if p := h.peer(pid); p != nil {
				p.dispatchInput(&msg) // фолбэк, если DataChannel ещё не открыт
			}
		default:
			log.Printf("signaling: unknown message type %q", msg.Type)
		}
	}
}

// peer возвращает зрителя по pid (или nil).
func (h *hub) peer(pid string) *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers[pid]
}

// onHello подключает зрителя: при первом — задаёт настройки трансляции и
// стартует общий захват; при последующих — берёт текущие (липкие) настройки.
func (h *hub) onHello(pid string, msg signalMessage) {
	h.mu.Lock()
	// Уже есть peer с таким pid?
	if p := h.peers[pid]; p != nil {
		switch p.pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected:
			// Реальное переподключение этого зрителя (reload) — пересоздаём.
			log.Printf("broker: viewer %s reconnect (%s) — recreating peer", pid, p.pc.ConnectionState())
			p.closePC()
			delete(h.peers, pid)
		default:
			h.mu.Unlock()
			return // дубль hello во время рукопожатия — игнор
		}
	}

	// Первый за всё время зритель задаёт настройки трансляции.
	if !h.configured {
		h.curOpts = h.buildOpts(msg)
		h.configured = true
	}
	// Захват ещё не идёт (первый зритель сейчас, либо все уходили и вернулись) —
	// поднимаем его с текущими (липкими) настройками.
	if h.str == nil {
		if err := h.startCaptureLocked(h.curOpts); err != nil {
			h.mu.Unlock()
			log.Printf("signaling: start capture: %v", err)
			return
		}
	}

	p := &peer{h: h, pid: pid}
	if err := p.buildLocked(); err != nil {
		h.mu.Unlock()
		log.Printf("signaling: build peer %s: %v", pid, err)
		return
	}
	h.peers[pid] = p
	opts := h.curOpts
	h.mu.Unlock()

	p.offer()
	// Сообщаем зрителю текущие настройки трансляции (его UI синхронизируется,
	// а не сбрасывает поток под свои дефолты).
	h.send(optsToState(opts), pid)
}

// startCaptureLocked создаёт общие видео/аудио-треки и стартует ОДИН захват под
// ними. Вызывать под h.mu.
func (h *hub) startCaptureLocked(opts capture.Options) error {
	vtrack, atrack, err := newSharedTracks(opts)
	if err != nil {
		return err
	}
	str := newStreamer(h.ctx, h.enc, vtrack, atrack)
	if err := str.reconfigure(opts); err != nil {
		return err
	}
	h.vtrack = vtrack
	h.atrack = atrack
	h.str = str
	h.setSource(opts.SourceKind, opts.SourceID)
	return nil
}

// stopCaptureLocked останавливает общий захват (когда не осталось зрителей).
// Вызывать под h.mu.
func (h *hub) stopCaptureLocked() {
	if h.str != nil {
		h.str.stop()
		h.str = nil
	}
	h.vtrack = nil
	h.atrack = nil
}

// removePeer закрывает зрителя и, если он был последним, гасит общий захват.
// Настройки (curOpts) сохраняются — следующий зритель подхватит их.
func (h *hub) removePeer(pid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.peers[pid]
	if p == nil {
		return
	}
	p.closePC()
	delete(h.peers, pid)
	log.Printf("broker: viewer %s left (%d remaining)", pid, len(h.peers))
	if len(h.peers) == 0 {
		h.stopCaptureLocked()
		log.Printf("broker: no viewers — capture stopped (settings kept)")
	}
}

// onConfig применяет настройки захвата (источник/разрешение/fps/битрейт/курсор)
// к ОБЩЕМУ потоку — изменение видят все зрители. Смена кодека/аудио идёт через
// "renegotiate" (требует новых треков). Без SDP-ренеготиации.
func (h *hub) onConfig(msg signalMessage) {
	if msg.Config == nil {
		return
	}
	h.mu.Lock()
	if h.str == nil {
		h.mu.Unlock()
		return
	}
	newOpts := msg.Config.apply(h.curOpts)
	h.curOpts = newOpts
	str := h.str
	h.mu.Unlock()

	// Настройки общие — синхронизируем панель у ВСЕХ зрителей (не только у того,
	// кто менял), чтобы их UI совпадал с новым потоком.
	h.broadcastState()

	// Перезапуск ffmpeg/SCK может занять ~секунду — в фоне, чтобы не блокировать
	// чтение WS (и приём ICE).
	go func() {
		if err := str.reconfigure(newOpts); err != nil {
			log.Printf("signaling: reconfigure: %v", err)
		}
		h.mu.Lock()
		h.setSource(newOpts.SourceKind, newOpts.SourceID)
		h.mu.Unlock()
	}()
}

// onRenegotiate меняет кодек/аудио ОБЩЕГО потока: пересоздаёт общие треки и
// шлёт оффер ренеготиации каждому зрителю — на тех же PeerConnection (без
// разрыва). Видео-трек всегда новый (новый SSRC → Chrome поднимает свежий
// декодер; H264 иначе виснет), Opus добавляется/снимается по opts.Audio.
func (h *hub) onRenegotiate(msg signalMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.str == nil {
		return
	}
	opts := h.applyHello(h.curOpts, msg)
	h.curOpts = opts

	h.str.stop() // остановить захват перед подменой треков

	vtrack, atrack, err := newSharedTracks(opts)
	if err != nil {
		log.Printf("reneg: new tracks: %v", err)
		h.cnl()
		return
	}

	st := optsToState(opts)
	for pid, p := range h.peers {
		if p.videoSender != nil {
			_ = p.pc.RemoveTrack(p.videoSender)
			p.videoSender = nil
		}
		if p.audioSender != nil {
			_ = p.pc.RemoveTrack(p.audioSender)
			p.audioSender = nil
		}
		vsender, err := p.pc.AddTrack(vtrack)
		if err != nil {
			log.Printf("reneg: add video track (%s): %v", pid, err)
			continue
		}
		go readRTCP(vsender)
		p.videoSender = vsender
		if atrack != nil {
			asender, err := p.pc.AddTrack(atrack)
			if err != nil {
				log.Printf("reneg: add audio track (%s): %v", pid, err)
			} else {
				go readRTCP(asender)
				p.audioSender = asender
			}
		}
		p.offer()
		h.send(st, pid) // синхронизируем панель зрителя с новым кодеком/аудио
	}

	h.vtrack = vtrack
	h.atrack = atrack
	h.str.setTracks(vtrack, atrack)
	log.Printf("broker: renegotiating all viewers (codec=%s audio=%v)", opts.Codec, opts.Audio)

	// Рестарт захвата под новые треки — в фоне (~1с), чтобы не держать h.mu.
	str := h.str
	go func() {
		if err := str.reconfigure(opts); err != nil {
			log.Printf("reneg: reconfigure: %v", err)
		}
		h.mu.Lock()
		h.setSource(opts.SourceKind, opts.SourceID)
		h.mu.Unlock()
	}()
}

// broadcastState рассылает текущие настройки трансляции всем зрителям, чтобы их
// панели совпадали с общим потоком после изменения настроек кем-то одним.
func (h *hub) broadcastState() {
	h.mu.Lock()
	st := optsToState(h.curOpts)
	pids := make([]string, 0, len(h.peers))
	for pid := range h.peers {
		pids = append(pids, pid)
	}
	h.mu.Unlock()
	for _, pid := range pids {
		h.send(st, pid)
	}
}

// buildOpts строит опции из hello первого зрителя поверх базовых.
func (h *hub) buildOpts(msg signalMessage) capture.Options {
	return h.applyHello(h.base, msg)
}

// applyHello накладывает config/codec/audio из сообщения на заданные опции.
func (h *hub) applyHello(opts capture.Options, msg signalMessage) capture.Options {
	if msg.Config != nil {
		opts = msg.Config.apply(opts)
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

// setSource обновляет кэш геометрии общего источника (для координат мыши).
// Вызывать под h.mu (кроме горутин reconfigure, где берём srcMu отдельно).
func (h *hub) setSource(kind string, id int) {
	r, err := capture.SourceRect(kind, id)
	if err != nil {
		return
	}
	h.srcMu.Lock()
	h.rect = r
	h.srcMu.Unlock()
}

// newSharedTracks создаёт видео-трек (по кодеку) и, если включён звук, Opus-трек.
// Треки общие для всех зрителей — pion раздаёт WriteSample по всем PC, куда они
// добавлены.
func newSharedTracks(opts capture.Options) (*webrtc.TrackLocalStaticSample, *webrtc.TrackLocalStaticSample, error) {
	mime := webrtc.MimeTypeVP8
	if opts.Codec == capture.CodecH264 {
		mime = webrtc.MimeTypeH264
	}
	vtrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: mime}, "screen", "desktop")
	if err != nil {
		return nil, nil, fmt.Errorf("new video track: %w", err)
	}
	var atrack *webrtc.TrackLocalStaticSample
	if opts.Audio {
		// Отдельный streamID (не "desktop") → видео и аудио в РАЗНЫХ MediaStream,
		// Chrome не синхронит A/V и не раздувает видео-буфер под звук.
		atrack, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "audio")
		if err != nil {
			return nil, nil, fmt.Errorf("new audio track: %w", err)
		}
	}
	return vtrack, atrack, nil
}

// buildLocked создаёт PeerConnection зрителя поверх ОБЩИХ треков хаба, вешает
// data-каналы (input/term) и обработчики. Захват уже идёт. Вызывать под h.mu.
func (p *peer) buildLocked() error {
	h := p.h
	// Публичный STUN — собираем srflx-кандидаты для P2P через NAT. TURN пока нет.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	p.pc = pc

	vsender, err := pc.AddTrack(h.vtrack)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("add video track: %w", err)
	}
	go readRTCP(vsender)
	p.videoSender = vsender

	if h.atrack != nil {
		asender, err := pc.AddTrack(h.atrack)
		if err != nil {
			_ = pc.Close()
			return fmt.Errorf("add audio track: %w", err)
		}
		go readRTCP(asender)
		p.audioSender = asender
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		h.send(signalMessage{Type: "candidate", Candidate: &init}, p.pid)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("signaling: viewer %s state -> %s", p.pid, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			// Отвал одного зрителя НЕ рвёт трансляцию остальных — чистим только его.
			go h.removePeer(p.pid)
		}
	})

	// Каналы данных создаём ДО оффера → попадут в SDP.
	if dc, err := pc.CreateDataChannel("input", nil); err != nil {
		log.Printf("signaling: data channel: %v", err)
	} else {
		dc.OnOpen(func() {
			hn, _ := os.Hostname()
			if b, err := json.Marshal(signalMessage{Type: "hostinfo", OS: osLabel(), Hostname: hn}); err == nil {
				_ = dc.SendText(string(b))
			}
		})
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			var im signalMessage
			if json.Unmarshal(m.Data, &im) != nil {
				return
			}
			switch im.Type {
			case "sources":
				if src, err := capture.ListSources(); err == nil {
					if b, err := json.Marshal(signalMessage{Type: "sources", Sources: &src}); err == nil {
						_ = dc.SendText(string(b))
					}
				}
			case "activate":
				if im.PID > 0 {
					_ = capture.ActivateApp(im.PID)
				}
			case "config":
				p.h.onConfig(im) // зритель меняет общие настройки (источник/разрешение)
			case "renegotiate":
				p.h.onRenegotiate(im) // зритель меняет кодек/аудио
			default:
				p.dispatchInput(&im)
			}
		})
	}
	if dc, err := pc.CreateDataChannel("term", nil); err != nil {
		log.Printf("signaling: term channel: %v", err)
	} else {
		sharedTerminal.bind(dc) // терминал общий: PTY один на всех зрителей
	}
	return nil
}

// offer создаёт и отправляет оффер этому зрителю.
func (p *peer) offer() {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("signaling: create offer (%s): %v", p.pid, err)
		return
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		log.Printf("signaling: set local description (%s): %v", p.pid, err)
		return
	}
	p.h.send(signalMessage{Type: "offer", SDP: offer.SDP}, p.pid)
}

// closePC закрывает PeerConnection зрителя.
func (p *peer) closePC() {
	if p.pc != nil {
		if err := p.pc.Close(); err != nil {
			log.Printf("signaling: pc close (%s): %v", p.pid, err)
		}
		p.pc = nil
	}
}

// dispatchInput обрабатывает события ввода (mouse/scroll/cursor/key/type) — общий
// путь для DataChannel (основной) и WebSocket (фолбэк).
func (p *peer) dispatchInput(msg *signalMessage) {
	switch msg.Type {
	case "mouse":
		if msg.Mouse != nil {
			p.handleMouse(msg.Mouse)
		}
	case "scroll":
		if msg.Scroll != nil {
			scrollMouse(msg.Scroll.Dx, msg.Scroll.Dy)
		}
	case "cursor":
		// Курсор хоста общий для захвата — меняем на лету у всех.
		p.h.mu.Lock()
		str := p.h.str
		p.h.mu.Unlock()
		if str != nil && msg.Config != nil && msg.Config.Cursor != nil {
			str.updateCursor(*msg.Config.Cursor)
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

// handleMouse мапит нормализованные координаты в глобальные и инжектит событие.
// Геометрия источника общая (один захват), drag-состояние — своё на зрителя.
func (p *peer) handleMouse(m *mouseMsg) {
	p.h.srcMu.Lock()
	r := p.h.rect
	p.h.srcMu.Unlock()
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
		p.btnDown = button
		p.dragged = false
	case "up":
		// dragMouse шлём ТОЛЬКО если реально был drag (приходили move). Иначе
		// (чистый клик) — просто отпускаем: без события Dragged, чтобы тап не
		// принимался за перетаскивание/выделение.
		if p.btnDown != "" && p.dragged {
			dragMouse(x, y, p.btnDown)
		}
		mouseToggle(button, false)
		p.btnDown = ""
		p.dragged = false
	default: // move
		if p.btnDown != "" {
			dragMouse(x, y, p.btnDown) // зажата кнопка → drag-событие
			p.dragged = true
		} else {
			moveMouse(x, y)
		}
	}
}
