package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/vseplet/katana/proto/capture"
)

// streamer владеет одним VP8-треком и захватом под ним. Захват можно
// переконфигурировать на лету (другой fps/ширина/битрейт), не пересоздавая
// PeerConnection: VP8 меняет разрешение на ближайшем кейфрейме, браузер
// подхватывает новый размер автоматически.
type streamer struct {
	parent context.Context
	enc    capture.CaptureEncoder
	track  *webrtc.TrackLocalStaticSample

	mu     sync.Mutex
	cancel context.CancelFunc // останавливает текущий захват
	done   chan struct{}      // закрывается, когда писатель кадров вышел
}

func newStreamer(parent context.Context, enc capture.CaptureEncoder, track *webrtc.TrackLocalStaticSample) *streamer {
	return &streamer{parent: parent, enc: enc, track: track}
}

// reconfigure останавливает текущий захват (если был) и запускает новый
// с указанными опциями, продолжая писать в тот же трек.
func (s *streamer) reconfigure(opts capture.Options) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Останавливаем предыдущий захват и дожидаемся выхода писателя,
	// чтобы два писателя не чередовали кадры в одном треке.
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}

	ctx, cancel := context.WithCancel(s.parent)
	frames, err := s.enc.Start(ctx, opts)
	if err != nil {
		cancel()
		return fmt.Errorf("start capture: %w", err)
	}

	done := make(chan struct{})
	s.cancel = cancel
	s.done = done

	frameDur := time.Second / time.Duration(opts.FPS)
	go func() {
		defer close(done)
		var n int
		for frame := range frames {
			if err := s.track.WriteSample(media.Sample{Data: frame, Duration: frameDur}); err != nil {
				log.Printf("webrtc: write sample: %v", err)
				return
			}
			n++
			if n == 1 {
				log.Printf("webrtc: first frame on track") // подтверждаем, что кадры пошли
			}
		}
	}()

	return nil
}

// stop останавливает захват.
func (s *streamer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
}

// newPeerConnection создаёт PeerConnection с одним sendonly VP8-треком и
// streamer'ом под ним, запускает начальный захват с opts.
//
// Возвращает PeerConnection и streamer — вызывающий обязан Close()/stop() их.
func newPeerConnection(ctx context.Context, enc capture.CaptureEncoder, opts capture.Options) (*webrtc.PeerConnection, *streamer, error) {
	// Пустой конфиг: localhost, host-кандидаты, без STUN/TURN.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, fmt.Errorf("new peer connection: %w", err)
	}

	mime := webrtc.MimeTypeVP8
	if opts.Codec == capture.CodecH264 {
		mime = webrtc.MimeTypeH264
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: mime},
		"screen", "desktop")
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("new track: %w", err)
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, nil, fmt.Errorf("add track: %w", err)
	}

	// Обязательно вычитывать RTCP, иначе буфер обратной связи pion переполнится.
	// PLI в прототипе не обрабатываем (кейфреймы периодические), но читаем.
	go readRTCP(sender)

	s := newStreamer(ctx, enc, track)
	if err := s.reconfigure(opts); err != nil {
		_ = pc.Close()
		return nil, nil, err
	}

	return pc, s, nil
}

// readRTCP вычитывает RTCP-пакеты от зрителя. Без этого pion переполняет
// внутренний буфер. PLI логируем, но не обрабатываем (см. §7 ТЗ).
func readRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return // sender закрыт вместе с PeerConnection
		}
	}
}
