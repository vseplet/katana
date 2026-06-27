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
// Тип "config" — расширение поверх ТЗ: смена параметров захвата на лету.
type signalMessage struct {
	Type      string                   `json:"type"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Config    *configMsg               `json:"config,omitempty"`
}

// configMsg — настройки захвата, присылаемые браузером. Указатели, чтобы
// отличать «не задано» от нуля; незаданные поля сохраняют текущее значение.
type configMsg struct {
	Screen      *int  `json:"screen,omitempty"`
	Width       *int  `json:"width,omitempty"`
	FPS         *int  `json:"fps,omitempty"`
	BitrateKbps *int  `json:"bitrateKbps,omitempty"`
	Threads     *int  `json:"threads,omitempty"`
	DropLate    *bool `json:"dropLate,omitempty"`
}

// apply накладывает настройки на базовые опции с клампингом разумных границ.
// Битрейт приходит числом (kbps) — не строкой, чтобы не пробрасывать
// произвольный текст в аргументы ffmpeg.
func (c *configMsg) apply(base capture.Options) capture.Options {
	o := base
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
	o := c.apply(base)
	switch q.Get("codec") {
	case "h264":
		o.Codec = capture.CodecH264
	case "vp8":
		o.Codec = capture.CodecVP8
	}
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
			}()
		default:
			log.Printf("signaling: unknown message type %q", msg.Type)
		}
	}
}
