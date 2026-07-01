//go:build darwin

package capture

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework AppKit -framework ScreenCaptureKit -framework CoreGraphics -framework CoreMedia -framework CoreVideo
#include <stdlib.h>

char *sck_list_sources(void);
int sck_source_size(int kind, unsigned int sid, int *outW, int *outH);
int sck_source_rect(int kind, unsigned int sid, double *x, double *y, double *w, double *h);
int sck_start(int kind, unsigned int sid, int fps, int handle, int audio, int cursor, int outW, int outH);
void sck_stop(int handle);
int sck_set_cursor(int handle, int show);
void inject_scroll(int dx, int dy);
int activate_app(int pid);
*/
import "C"

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// sckSink хранит последний кадр потока SCK. SCK отдаёт кадры только при
// изменении содержимого, поэтому держим последний и гоним его тикером —
// ffmpeg получает ровный поток даже на статичном окне.
type sckSink struct {
	mu     sync.Mutex
	latest []byte    // последний видео-кадр (для тикера CFR)
	audio  io.Writer // stdin аудио-ffmpeg (ffmpeg-режим; nil иначе)
	// Нативный режим (без ffmpeg): VideoToolbox + Opus.
	vt      *vtEncoder
	opus    *opusEncoder
	audioCh chan []byte
	restart chan struct{} // сигнал: SCK-поток остановился, надо пересоздать захват
}

var (
	sckMu    sync.Mutex
	sckSinks = map[int]*sckSink{}
	sckSeq   int
)

func sckRegister() (int, *sckSink) {
	sckMu.Lock()
	defer sckMu.Unlock()
	sckSeq++
	s := &sckSink{restart: make(chan struct{}, 1)}
	sckSinks[sckSeq] = s
	return sckSeq, s
}

func sckUnregister(handle int) {
	sckMu.Lock()
	delete(sckSinks, handle)
	sckMu.Unlock()
}

//export goSCKFrame
func goSCKFrame(handle C.int, buf unsafe.Pointer, length C.int) {
	sckMu.Lock()
	s := sckSinks[int(handle)]
	sckMu.Unlock()
	if s == nil {
		return
	}
	// Копируем: C-буфер освободят сразу после возврата из этой функции.
	frame := C.GoBytes(buf, length)
	s.mu.Lock()
	s.latest = frame
	s.mu.Unlock()
}

//export goSCKStopped
func goSCKStopped(handle C.int) {
	sckMu.Lock()
	s := sckSinks[int(handle)]
	sckMu.Unlock()
	if s == nil || s.restart == nil {
		return
	}
	select {
	case s.restart <- struct{}{}: // будим горутину рестарта (неблокирующе)
	default:
	}
}

//export goSCKAudio
func goSCKAudio(handle C.int, buf unsafe.Pointer, length C.int) {
	sckMu.Lock()
	s := sckSinks[int(handle)]
	sckMu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Нативный режим: PCM (f32 interleaved) → Opus → канал.
	if s.opus != nil {
		pcm := unsafe.Slice((*float32)(buf), int(length)/4)
		for _, pkt := range s.opus.feed(pcm) {
			if s.audioCh != nil {
				select {
				case s.audioCh <- pkt:
				default:
				}
			}
		}
		return
	}
	// ffmpeg-режим: пишем PCM в stdin (C-буфер валиден до возврата).
	if s.audio != nil {
		_, _ = s.audio.Write(unsafe.Slice((*byte)(buf), int(length)))
	}
}

// sckKindCode переводит строковый вид источника в код для нативной стороны.
func sckKindCode(kind string) int {
	switch kind {
	case "window":
		return 1
	case "app":
		return 2
	default:
		return 0 // display
	}
}

func sckSourceSize(kind, id int) (int, int, error) {
	var w, h C.int
	if rc := C.sck_source_size(C.int(kind), C.uint(id), &w, &h); rc != 0 {
		return 0, 0, fmt.Errorf("sck_source_size rc=%d", int(rc))
	}
	return int(w), int(h), nil
}

func sckStart(kind, id, fps, handle int, audio, cursor bool, outW, outH int) error {
	if rc := C.sck_start(C.int(kind), C.uint(id), C.int(fps), C.int(handle),
		C.int(b2i(audio)), C.int(b2i(cursor)), C.int(outW), C.int(outH)); rc != 0 {
		return fmt.Errorf("sck_start rc=%d", int(rc))
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// bitrateKbps парсит строку битрейта ("3M" | "3000k" | "3000") в килобиты/с.
func bitrateKbps(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 3000
	}
	mul := 1
	switch s[len(s)-1] {
	case 'M', 'm':
		mul = 1000
		s = s[:len(s)-1]
	case 'k', 'K':
		s = s[:len(s)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 3000
	}
	return n * mul
}

func sckStop(handle int) { C.sck_stop(C.int(handle)) }

func sckSetCursor(handle int, show bool) { C.sck_set_cursor(C.int(handle), C.int(b2i(show))) }

// startSCK захватывает дисплей/окно/приложение через ScreenCaptureKit. H264 —
// нативно (VideoToolbox + Opus, без ffmpeg); VP8 — через ffmpeg.
func startSCK(ctx context.Context, opts Options) (*Stream, error) {
	if opts.Codec == CodecH264 {
		return startSCKNative(ctx, opts)
	}
	return startSCKFFmpeg(ctx, opts)
}

// startSCKNative кодирует H264 аппаратно (VideoToolbox) и Opus (libopus) прямо
// в процессе — ffmpeg не нужен. SCK сам масштабирует кадр до целевого размера.
func startSCKNative(ctx context.Context, opts Options) (*Stream, error) {
	kind := sckKindCode(opts.SourceKind)
	srcW, srcH, err := sckSourceSize(kind, opts.SourceID)
	if err != nil {
		return nil, fmt.Errorf("sck source size: %w", err)
	}
	// Целевой размер: native (Width=0) или даунскейл по ширине (чётные размеры).
	outW, outH := srcW, srcH
	if opts.Width > 0 && opts.Width < srcW {
		outW = opts.Width &^ 1
		outH = (srcH * outW / srcW) &^ 1
	}

	handle, sink := sckRegister()
	vt, err := newVTEncoder(handle, outW, outH, opts.FPS, bitrateKbps(opts.Bitrate))
	if err != nil {
		sckUnregister(handle)
		return nil, fmt.Errorf("vt encoder: %w", err)
	}

	frames := make(chan []byte, 8)
	nativeRegister(handle, frames)

	var audioCh chan []byte
	if opts.Audio {
		if oe, oerr := newOpusEncoder(); oerr != nil {
			log.Printf("capture: opus: %v (continuing without audio)", oerr)
		} else {
			audioCh = make(chan []byte, 16)
			sink.mu.Lock()
			sink.opus = oe
			sink.audioCh = audioCh
			sink.mu.Unlock()
		}
	}
	sink.mu.Lock()
	sink.vt = vt
	sink.mu.Unlock()

	log.Printf("capture: sck %s/%d %dx%d audio=%v | native VideoToolbox H264",
		opts.SourceKind, opts.SourceID, outW, outH, opts.Audio)

	if err := sckStart(kind, opts.SourceID, opts.FPS, handle, opts.Audio, opts.Cursor, outW, outH); err != nil {
		nativeClose(handle, frames)
		sckUnregister(handle)
		vt.close()
		if sink.opus != nil {
			sink.opus.close()
		}
		return nil, fmt.Errorf("sck start: %w", err)
	}

	// Восстановление захвата: SCK останавливается при сне/пробуждении Mac и смене
	// дисплея (goSCKStopped через делегат). Тикер CFR при этом гнал бы последний
	// кадр вечно — зритель видел бы замороженную картинку на 60 fps. Пересоздаём
	// SCK-поток на том же handle: sink/энкодер/канал сохраняются, кадры снова
	// пойдут в goSCKFrame.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sink.restart:
				log.Printf("capture: SCK stopped — restarting screen capture")
				sckStop(handle) // остановить/убрать старый поток
				select { // проглотить возможный само-сигнал от sckStop (не зациклиться)
				case <-sink.restart:
				default:
				}
				for ctx.Err() == nil {
					if err := sckStart(kind, opts.SourceID, opts.FPS, handle, opts.Audio, opts.Cursor, outW, outH); err != nil {
						log.Printf("capture: SCK restart failed: %v (retry in 1s)", err)
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Second):
						}
						continue
					}
					log.Printf("capture: screen capture restarted")
					break
				}
			}
		}
	}()

	// Тикер: гоним последний кадр в VT с целевым FPS (ровный CFR даже на статике).
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(opts.FPS))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sink.mu.Lock()
				f := sink.latest
				sink.mu.Unlock()
				if f != nil {
					vt.encode(f)
				}
			}
		}
	}()

	// Очистка по отмене контекста.
	go func() {
		<-ctx.Done()
		sckStop(handle)
		sckUnregister(handle)
		sink.mu.Lock()
		oe := sink.opus
		sink.opus = nil
		sink.mu.Unlock()
		vt.close()
		if oe != nil {
			oe.close()
		}
		nativeClose(handle, frames) // удаляет из реестра и закрывает канал под мьютексом
		if audioCh != nil {
			sink.mu.Lock()
			ac := sink.audioCh
			sink.audioCh = nil
			sink.mu.Unlock()
			if ac != nil {
				close(ac)
			}
		}
		log.Printf("capture stopped (sck native)")
	}()

	return &Stream{
		Video:     frames,
		Audio:     audioCh,
		SetCursor: func(show bool) { sckSetCursor(handle, show) },
		ForceKeyframe: func() {
			sink.mu.Lock()
			v := sink.vt
			sink.mu.Unlock()
			if v != nil {
				v.requestKeyframe()
			}
		},
		SetBitrate: func(kbps int) {
			sink.mu.Lock()
			v := sink.vt
			sink.mu.Unlock()
			if v != nil {
				v.setBitrate(kbps)
			}
		},
	}, nil
}

// startSCKFFmpeg — путь через ffmpeg (для VP8): SCK отдаёт BGRA в stdin
// видео-ffmpeg (скейл/энкод), при opts.Audio — PCM в stdin аудио-ffmpeg.
func startSCKFFmpeg(ctx context.Context, opts Options) (*Stream, error) {
	ff := FFmpegPath()
	if ff == "" {
		return nil, errNoFFmpeg
	}
	kind := sckKindCode(opts.SourceKind)
	w, h, err := sckSourceSize(kind, opts.SourceID)
	if err != nil {
		return nil, fmt.Errorf("sck source size: %w", err)
	}

	args := buildSCKArgs(opts, w, h)
	cmd := exec.CommandContext(ctx, ff, args...)
	log.Printf("capture: sck %s/%d %dx%d audio=%v | ffmpeg %s",
		opts.SourceKind, opts.SourceID, w, h, opts.Audio, strings.Join(args, " "))

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	// Регистрируем sink ДО старта SCK, чтобы первые кадры не потерялись.
	handle, sink := sckRegister()

	// Аудио (опционально): отдельный ffmpeg PCM→Opus, его stdin вешаем в sink.
	var audioCh chan []byte
	var audioStop func()
	if opts.Audio {
		ai, ach, stop, aerr := startAudioEncoder(ctx)
		if aerr != nil {
			log.Printf("capture: audio encoder: %v (continuing without audio)", aerr)
		} else {
			sink.mu.Lock()
			sink.audio = ai
			sink.mu.Unlock()
			audioCh = ach
			audioStop = stop
		}
	}

	if err := sckStart(kind, opts.SourceID, opts.FPS, handle, opts.Audio, opts.Cursor, 0, 0); err != nil {
		sckUnregister(handle)
		_ = stdin.Close()
		if audioStop != nil {
			audioStop()
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("sck start: %w", err)
	}

	// Тикер: гоним последний видео-кадр с целевой частотой — ровный CFR-вход
	// даже когда SCK молчит (статичное окно). Аудио пишется напрямую в goSCKAudio.
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(opts.FPS))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sink.mu.Lock()
				f := sink.latest
				sink.mu.Unlock()
				if f == nil {
					continue
				}
				if _, err := stdin.Write(f); err != nil {
					return
				}
			}
		}
	}()

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer func() {
			sckStop(handle)
			sckUnregister(handle)
			_ = stdin.Close()
			if audioStop != nil {
				audioStop()
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			log.Printf("capture stopped (sck)")
		}()

		in := bufio.NewReader(stdout)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()

	return &Stream{
		Video:     frames,
		Audio:     audioCh,
		SetCursor: func(show bool) { sckSetCursor(handle, show) },
	}, nil
}

// startAudioEncoder поднимает ffmpeg PCM(f32le 48k стерео)→Opus(ogg), читает
// stdout через oggreader и отдаёт Opus-пакеты в канал. Возвращает stdin (куда
// SCK пишет PCM), канал пакетов и функцию остановки.
func startAudioEncoder(ctx context.Context) (io.Writer, chan []byte, func(), error) {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "f32le", "-ar", "48000", "-ac", "2", "-i", "-",
		"-c:a", "libopus", "-b:a", "128k", "-application", "lowdelay",
		"-page_duration", "20000", // одна opus-страница ≈ 20 мс
		"-f", "ogg", "-",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	go logStderr(stderr)
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}

	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		reader, _, err := oggreader.NewWith(bufio.NewReader(stdout))
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("capture: ogg header: %v", err)
			}
			return
		}
		for {
			page, _, err := reader.ParseNextPage()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("audio: ogg read: %v", err)
				}
				return
			}
			select {
			case out <- page:
			case <-ctx.Done():
				return
			}
		}
	}()

	stop := func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	return stdin, out, stop, nil
}

// SourceDisplay — дисплей (для SCK-захвата всего экрана).
type SourceDisplay struct {
	ID     int `json:"id"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// SourceWindow — отдельное окно.
type SourceWindow struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	App    string `json:"app"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// SourceApp — запущенное приложение (захват всех его окон).
type SourceApp struct {
	PID      int    `json:"pid"`
	BundleID string `json:"bundleId"`
	Name     string `json:"name"`
}

// Sources — всё, что можно захватить через ScreenCaptureKit.
type Sources struct {
	Displays []SourceDisplay `json:"displays"`
	Windows  []SourceWindow  `json:"windows"`
	Apps     []SourceApp     `json:"apps"`
}

// Rect — глобальный прямоугольник источника (точки, top-left).
type Rect struct {
	X, Y, W, H float64
}

// InjectScroll постит пиксельно-точный скролл (как трекпад): dx — горизонталь,
// dy — вертикаль, в пикселях.
func InjectScroll(dx, dy int) { C.inject_scroll(C.int(dx), C.int(dy)) }

// ActivateApp выводит приложение (по pid) на передний план на хосте.
func ActivateApp(pid int) error {
	if rc := C.activate_app(C.int(pid)); rc != 0 {
		return fmt.Errorf("activate_app rc=%d", int(rc))
	}
	return nil
}

// SourceRect возвращает положение/размер источника на экране — для маппинга
// координат мыши из браузера в глобальные координаты. kind: window|app|display.
func SourceRect(kind string, id int) (Rect, error) {
	var x, y, w, h C.double
	rc := C.sck_source_rect(C.int(sckKindCode(kind)), C.uint(id), &x, &y, &w, &h)
	if rc != 0 {
		return Rect{}, fmt.Errorf("sck_source_rect rc=%d", int(rc))
	}
	return Rect{X: float64(x), Y: float64(y), W: float64(w), H: float64(h)}, nil
}

// ListSources перечисляет источники захвата через ScreenCaptureKit.
// Требует разрешения на запись экрана — без него заголовки окон будут пустыми.
func ListSources() (Sources, error) {
	cs := C.sck_list_sources()
	defer C.free(unsafe.Pointer(cs))

	var s Sources
	if err := json.Unmarshal([]byte(C.GoString(cs)), &s); err != nil {
		return Sources{}, fmt.Errorf("parse sck sources: %w", err)
	}
	return s, nil
}
