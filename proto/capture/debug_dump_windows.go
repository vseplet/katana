//go:build windows

package capture

// Диагностика разрыва кадра: по env KATANA_DUMP_FRAMES=1 периодически сохраняем
// захваченный BGRA-кадр (ДО конвертации в NV12 и энкода) в PNG. Если в дампе
// виден горизонтальный шов (окно в двух позициях), значит рвётся сам захват WGC
// (гонка чтения текстуры пула), а не тайминг/энкод/сеть. Кольцо из N файлов —
// последние кадры drag'а всегда на диске. Полностью выключено без env.

import (
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sync"
	"unsafe"
)

var (
	dumpOnce    sync.Once
	dumpEnabled bool
	dumpDir     string
)

// debugCapture — включены ли подробные диагностические логи Windows-захвата
// (перечень энкодеров, указатели COM, интервал кейфреймов, конфиг ICodecAPI).
// Гейт по env KATANA_DEBUG — в обычном режиме лог чистый.
func debugCapture() bool { return os.Getenv("KATANA_DEBUG") != "" }

const (
	dumpEveryN = 6  // сохраняем каждый N-й новый кадр
	dumpRing   = 16 // кольцо файлов frame-00..frame-15
)

func dumpInit() {
	dumpOnce.Do(func() {
		if os.Getenv("KATANA_DUMP_FRAMES") == "" {
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			log.Printf("capture/dump: UserHomeDir: %v", err)
			return
		}
		dumpDir = filepath.Join(home, ".katana", "dump")
		if err := os.MkdirAll(dumpDir, 0o755); err != nil {
			log.Printf("capture/dump: mkdir %s: %v", dumpDir, err)
			return
		}
		dumpEnabled = true
		log.Printf("capture/dump: ВКЛючён дамп кадров → %s (каждый %d-й, кольцо %d)",
			dumpDir, dumpEveryN, dumpRing)
	})
}

// maybeDumpBGRA сохраняет кадр, если включён дамп и подошёл счётчик. data/rowPitch —
// как из mapStaging (BGRA, stride в байтах), w×h — размер кадра WGC.
func maybeDumpBGRA(counter *int, ring *int, data uintptr, rowPitch, w, h int) {
	if !dumpEnabled {
		return
	}
	*counter++
	if *counter%dumpEveryN != 0 {
		return
	}
	idx := *ring
	*ring = (*ring + 1) % dumpRing

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	src := unsafe.Slice((*byte)(unsafe.Pointer(data)), rowPitch*h)
	for y := 0; y < h; y++ {
		row := src[y*rowPitch:]
		di := y * img.Stride
		for x := 0; x < w; x++ {
			b := row[x*4+0]
			g := row[x*4+1]
			r := row[x*4+2]
			img.Pix[di+0] = r
			img.Pix[di+1] = g
			img.Pix[di+2] = b
			img.Pix[di+3] = 255
			di += 4
		}
	}
	path := filepath.Join(dumpDir, "frame-"+twoDigits(idx)+".png")
	f, err := os.Create(path)
	if err != nil {
		return
	}
	_ = png.Encode(f, img)
	_ = f.Close()
}

func twoDigits(n int) string {
	return string([]byte{byte('0' + (n/10)%10), byte('0' + n%10)})
}
