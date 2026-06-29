package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/rtcp"
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
	track  *webrtc.TrackLocalStaticSample // видео
	audio  *webrtc.TrackLocalStaticSample // Opus, nil если звук выключен

	mu          sync.Mutex
	cancel      context.CancelFunc // останавливает текущий захват
	done        chan struct{}      // закрывается, когда писатели кадров вышли
	setCursor   func(bool)         // живое переключение курсора хоста (без рестарта)
	forceKeyFn  func()             // форс keyframe у энкодера (по PLI); nil если не поддерж.
	setBitrate  func(kbps int)     // смена битрейта энкодера на лету; nil если не поддерж.
}

func newStreamer(parent context.Context, enc capture.CaptureEncoder, track, audio *webrtc.TrackLocalStaticSample) *streamer {
	return &streamer{parent: parent, enc: enc, track: track, audio: audio}
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
	stream, err := s.enc.Start(ctx, opts)
	if err != nil {
		cancel()
		return fmt.Errorf("start capture: %w", err)
	}

	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.setCursor = stream.SetCursor
	s.forceKeyFn = stream.ForceKeyframe
	s.setBitrate = stream.SetBitrate

	var wg sync.WaitGroup

	// Видео. На ошибке WriteSample НЕ выходим (иначе одна транзиентная ошибка
	// навсегда останавливает видео — картинка застывает, аудио идёт, лечит
	// только перезагрузка). Пропускаем кадр и продолжаем; восстановимся на
	// ближайшем кейфрейме. Логируем первую ошибку, дальше молча.
	wg.Add(1)
	frameDur := time.Second / time.Duration(opts.FPS)
	go func() {
		defer wg.Done()
		var n int
		var loggedErr bool
		for frame := range stream.Video {
			if err := s.track.WriteSample(media.Sample{Data: frame, Duration: frameDur}); err != nil {
				// Не выходим на транзиентной ошибке (иначе видео встанет навсегда):
				// логируем один раз и продолжаем, восстановимся на кейфрейме.
				if !loggedErr {
					loggedErr = true
					log.Printf("webrtc: write video: %v (continuing)", err)
				}
				continue
			}
			n++
			if n == 1 {
				log.Printf("webrtc: first frame on track")
			}
		}
	}()

	// Аудио (Opus, ~20 мс на пакет) — если есть трек и поток.
	if s.audio != nil && stream.Audio != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var loggedErr bool
			for pkt := range stream.Audio {
				if err := s.audio.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond}); err != nil {
					if !loggedErr {
						loggedErr = true
						log.Printf("webrtc: write audio: %v (continuing)", err)
					}
					continue
				}
			}
		}()
	}

	go func() { wg.Wait(); close(done) }()
	return nil
}

// setTracks подменяет треки, в которые пишет захват (для ренеготиации: новый
// видео-трек с новым SSRC и/или добавление/снятие аудио). Вызывать ТОЛЬКО при
// остановленном захвате (между stop и reconfigure), иначе писатель гонится.
func (s *streamer) setTracks(video, audio *webrtc.TrackLocalStaticSample) {
	s.mu.Lock()
	s.track = video
	s.audio = audio
	s.mu.Unlock()
}

// updateCursor переключает видимость курсора хоста НА ЛЕТУ, без перезапуска
// захвата (иначе каждый тоггл режима управления = ~1с обрыв видео).
func (s *streamer) updateCursor(show bool) {
	s.mu.Lock()
	fn := s.setCursor
	s.mu.Unlock()
	if fn != nil {
		fn(show)
	}
}

// requestKeyframe просит энкодер выдать keyframe (ответ на PLI зрителя).
func (s *streamer) requestKeyframe() {
	s.mu.Lock()
	fn := s.forceKeyFn
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// setBitrateKbps меняет битрейт энкодера на лету (адаптация к сети).
func (s *streamer) setBitrateKbps(kbps int) {
	s.mu.Lock()
	fn := s.setBitrate
	s.mu.Unlock()
	if fn != nil {
		fn(kbps)
	}
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

// readRTCP вычитывает RTCP-пакеты от зрителя (иначе pion переполняет внутренний
// буфер). На PLI (зритель потерял кадр, просит keyframe чтобы дропнуть буфер и
// догнать live) дёргаем onPLI; на ReceiverReport отдаём долю потерь (0..1) в
// onLoss для адаптации битрейта. Любой колбэк может быть nil.
func readRTCP(sender *webrtc.RTPSender, onPLI func(), onLoss func(lost float64)) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return // sender закрыт вместе с PeerConnection
		}
		for _, p := range pkts {
			switch pkt := p.(type) {
			case *rtcp.PictureLossIndication:
				if onPLI != nil {
					onPLI()
				}
			case *rtcp.ReceiverReport:
				if onLoss != nil {
					for _, r := range pkt.Reports {
						onLoss(float64(r.FractionLost) / 256.0)
					}
				}
			}
		}
	}
}
