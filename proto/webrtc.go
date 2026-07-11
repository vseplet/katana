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

	mu         sync.Mutex
	cancel     context.CancelFunc // останавливает текущий захват
	done       chan struct{}      // закрывается, когда писатели кадров вышли
	setCursor  func(bool)         // живое переключение курсора хоста (без рестарта)
	forceKeyFn func()             // форс keyframe у энкодера (по PLI); nil если не поддерж.
	setBitrate func(kbps int)     // смена битрейта энкодера на лету; nil если не поддерж.

	// Коалесинг живых переконфигураций. Быстрый перебор (слайдер разрешения и др.)
	// схлопываем в ОДИН рестарт захвата после паузы: каждый рестарт пере-подключает
	// PipeWire к ScreenCast-ноде KWin, а та от частых пере-подключений деградирует —
	// поток «тормозит наглухо» до перезапуска хоста. Первый (стартовый) вызов —
	// синхронный (нужна ошибка старта); живые — дебаунс. rcGen инвалидирует
	// отложенный таймер при stop()/новом вызове.
	rcMu      sync.Mutex
	rcStarted bool
	rcPending *capture.Options
	rcTimer   *time.Timer
	rcGen     uint64
}

func newStreamer(parent context.Context, enc capture.CaptureEncoder, track, audio *webrtc.TrackLocalStaticSample) *streamer {
	return &streamer{parent: parent, enc: enc, track: track, audio: audio}
}

// reconfigure применяет новые опции захвата. Первый вызов (старт потока) —
// синхронный, чтобы вернуть ошибку старта. Последующие живые смены ДЕБАУНСЯТСЯ и
// коалесятся: быстрая серия (перебор слайдера) даёт ОДИН рестарт после паузы,
// а не N — иначе частый ре-коннект PipeWire убивает ScreenCast-ноду KWin.
func (s *streamer) reconfigure(opts capture.Options) error {
	s.rcMu.Lock()
	if !s.rcStarted {
		s.rcStarted = true
		s.rcMu.Unlock()
		return s.applyReconfigure(opts) // старт — синхронно, с ошибкой
	}
	s.rcPending = &opts
	s.rcGen++
	gen := s.rcGen
	if s.rcTimer != nil {
		s.rcTimer.Stop()
	}
	s.rcTimer = time.AfterFunc(350*time.Millisecond, func() {
		s.rcMu.Lock()
		if gen != s.rcGen { // отменён stop()'ом или вытеснен новым вызовом
			s.rcMu.Unlock()
			return
		}
		p := s.rcPending
		s.rcPending = nil
		s.rcMu.Unlock()
		if p != nil {
			if err := s.applyReconfigure(*p); err != nil {
				log.Printf("streamer: live reconfigure: %v", err)
			}
		}
	})
	s.rcMu.Unlock()
	return nil
}

// applyReconfigure останавливает текущий захват (если был) и запускает новый
// с указанными опциями, продолжая писать в тот же трек.
func (s *streamer) applyReconfigure(opts capture.Options) error {
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
		// Диагностика третьей ступени конвейера (capture → encode → WriteSample):
		// частота записи в трек и максимальная блокировка WriteSample за окно.
		// Если WriteSample/с проседает при здоровом encode — затор в pion/пейсере.
		var wsN, wsBad int
		var wsMaxBlock time.Duration
		wsStat := time.Now()
		for frame := range stream.Video {
			if !validAU(frame) {
				wsBad++
			}
			t0 := time.Now()
			err := s.track.WriteSample(media.Sample{Data: frame, Duration: frameDur})
			if d := time.Since(t0); d > wsMaxBlock {
				wsMaxBlock = d
			}
			if err != nil {
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
			wsN++
			if el := time.Since(wsStat); el >= 2*time.Second {
				log.Printf("webrtc: 2с writeSample=%d/с maxBlock=%.0fмс badAU=%d",
					int(float64(wsN)/el.Seconds()), wsMaxBlock.Seconds()*1000, wsBad)
				wsN, wsBad, wsMaxBlock, wsStat = 0, 0, 0, time.Now()
			}
		}
	}()

	// Аудио (Opus, ~20 мс на пакет) — если есть трек и поток. Шлём как пришло:
	// RTP-метку pion двигает по Duration (не по времени прихода), так что пачечная
	// доставка меток не портит; тактование на нашей стороне (пробовали) только
	// добавляет задержку и провалы на андерранах.
	if s.audio != nil && stream.Audio != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var loggedErr bool
			for pkt := range stream.Audio {
				if err := s.audio.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond}); err != nil && !loggedErr {
					loggedErr = true
					log.Printf("webrtc: write audio: %v (continuing)", err)
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
	setPacerBitrateKbps(kbps) // пейсер держит ×2.5 текущего битрейта
	s.mu.Lock()
	fn := s.setBitrate
	s.mu.Unlock()
	if fn != nil {
		fn(kbps)
	}
}

// stop останавливает захват.
func (s *streamer) stop() {
	// Гасим отложенную живую переконфигурацию: rcGen++ инвалидирует уже
	// взведённый таймер (его колбэк увидит gen != s.rcGen и выйдет), чтобы
	// финальный teardown не породил спонтанный рестарт захвата.
	s.rcMu.Lock()
	s.rcGen++
	s.rcPending = nil
	if s.rcTimer != nil {
		s.rcTimer.Stop()
	}
	s.rcMu.Unlock()

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

// validAU — диагностика целостности H264 Access Unit перед WriteSample (см.
// webrtc: 2с ... badAU=N). Валидный AU: начинается со старт-кода Annex-B и несёт
// РОВНО ОДИН видеокадр (один VCL NAL с first_mb_in_slice==0; NAL типов 1/5 может
// быть несколько — мультислайс, но старт кадра один). badAU>0 = кадры режутся по
// NAL или склеиваются — pion-пакетизатор при этом портит поток для декодера.
func validAU(au []byte) bool {
	frameStarts := 0
	n := len(au)
	for i := 0; i+3 < n; {
		// ищем старт-код 00 00 01 (с опциональным ведущим 00)
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			hdr := i + 3
			if hdr >= n {
				break
			}
			if i == 0 || (i >= 1 && au[i-1] == 0) || i == 1 {
				// ок: 3- или 4-байтовый старт-код
			}
			nalType := au[hdr] & 0x1F
			if nalType == 1 || nalType == 5 { // VCL slice
				if hdr+1 < n && au[hdr+1]&0x80 != 0 { // ue(v): first_mb_in_slice==0
					frameStarts++
				}
			}
			i = hdr + 1
			continue
		}
		i++
	}
	// старт-код в начале обязателен
	hasLead := n > 4 && ((au[0] == 0 && au[1] == 0 && au[2] == 1) ||
		(au[0] == 0 && au[1] == 0 && au[2] == 0 && au[3] == 1))
	return hasLead && frameStarts == 1
}
