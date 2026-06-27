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
	track  *webrtc.TrackLocalStaticSample // видео
	audio  *webrtc.TrackLocalStaticSample // Opus, nil если звук выключен

	mu     sync.Mutex
	cancel context.CancelFunc // останавливает текущий захват
	done   chan struct{}      // закрывается, когда писатели кадров вышли
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
		// Диагностика: сколько кадров реально ушло в трек за последние ~5с.
		// При зависании видно, падает ли выход сервера в 0 (стоп пайплайна)
		// или держится (тогда виноват приём/декод в браузере).
		var since int
		last := time.Now()
		for frame := range stream.Video {
			if err := s.track.WriteSample(media.Sample{Data: frame, Duration: frameDur}); err != nil {
				if !loggedErr {
					loggedErr = true
					log.Printf("webrtc: write video: %v (продолжаю)", err)
				}
				continue
			}
			n++
			since++
			if n == 1 {
				log.Printf("webrtc: first frame on track")
			}
			if d := time.Since(last); d >= 5*time.Second {
				log.Printf("webrtc: video out %.0f fps", float64(since)/d.Seconds())
				since = 0
				last = time.Now()
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
						log.Printf("webrtc: write audio: %v (продолжаю)", err)
					}
					continue
				}
			}
		}()
	}

	go func() { wg.Wait(); close(done) }()
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

	// Opus-аудиотрек — только если звук включён при подключении (как кодек,
	// смена требует переподключения: добавление трека = ренеготиация).
	var audio *webrtc.TrackLocalStaticSample
	if opts.Audio {
		audio, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
			"audio", "desktop")
		if err != nil {
			_ = pc.Close()
			return nil, nil, fmt.Errorf("new audio track: %w", err)
		}
		asender, err := pc.AddTrack(audio)
		if err != nil {
			_ = pc.Close()
			return nil, nil, fmt.Errorf("add audio track: %w", err)
		}
		go readRTCP(asender)
	}

	s := newStreamer(ctx, enc, track, audio)
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
