//go:build darwin

package capture

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework AppKit -framework ScreenCaptureKit -framework CoreGraphics -framework CoreMedia -framework CoreVideo
#include <stdlib.h>

char *sck_list_sources(void);
int sck_source_size(int kind, unsigned int sid, int *outW, int *outH);
int sck_start(int kind, unsigned int sid, int fps, int handle);
void sck_stop(int handle);
*/
import "C"

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// sckSink хранит последний кадр потока SCK. SCK отдаёт кадры только при
// изменении содержимого, поэтому держим последний и гоним его тикером —
// ffmpeg получает ровный поток даже на статичном окне.
type sckSink struct {
	mu     sync.Mutex
	latest []byte
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
	s := &sckSink{}
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

func sckStart(kind, id, fps, handle int) error {
	if rc := C.sck_start(C.int(kind), C.uint(id), C.int(fps), C.int(handle)); rc != 0 {
		return fmt.Errorf("sck_start rc=%d", int(rc))
	}
	return nil
}

func sckStop(handle int) { C.sck_stop(C.int(handle)) }

// startSCK захватывает окно/приложение через ScreenCaptureKit: нативный поток
// шлёт BGRA-кадры в stdin ffmpeg, тот скейлит/кодирует, мы читаем stdout.
func startSCK(ctx context.Context, opts Options) (<-chan []byte, error) {
	kind := sckKindCode(opts.SourceKind)
	w, h, err := sckSourceSize(kind, opts.SourceID)
	if err != nil {
		return nil, fmt.Errorf("sck source size: %w", err)
	}

	args := buildSCKArgs(opts, w, h)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	log.Printf("capture: sck %s/%d %dx%d | ffmpeg %s",
		opts.SourceKind, opts.SourceID, w, h, strings.Join(args, " "))

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
	if err := sckStart(kind, opts.SourceID, opts.FPS, handle); err != nil {
		sckUnregister(handle)
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("sck start: %w", err)
	}

	// Тикер: гоним последний кадр в ffmpeg с целевой частотой — ровный CFR-вход
	// даже когда SCK молчит (статичное окно).
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
					return // ffmpeg ушёл
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

	return frames, nil
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
