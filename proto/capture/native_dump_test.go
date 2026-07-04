//go:build linux && cgo

package capture

// Локальная валидация нативного захвата БЕЗ браузера/WebRTC: гоняет PipeWire→VAAPI
// энкод N секунд и пишет Annex-B H264 в файл для ffprobe. Скомпилировать в
// контейнере (go test -c), запускать на хосте (нужны Wayland/PipeWire/портал):
//
//   WAYLAND_DISPLAY=wayland-0 KATANA_DUMP=/tmp/out.h264 KATANA_DUMP_SECS=8 \
//     ./dumpcap.test -test.run TestNativeDump -test.v
//
// Появится диалог KDE «разрешить захват экрана» — подтвердить.

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestNativeDump(t *testing.T) {
	out := os.Getenv("KATANA_DUMP")
	if out == "" {
		t.Skip("set KATANA_DUMP=/path/out.h264")
	}
	secs := 8
	if v := os.Getenv("KATANA_DUMP_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			secs = n
		}
	}
	fps := 60
	if v := os.Getenv("KATANA_DUMP_FPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			fps = n
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := nativePipeWireCapture(ctx, Options{
		Codec: CodecH264, FPS: fps, Bitrate: "8M", Width: 0,
	})
	if err != nil {
		t.Fatalf("capture start: %v", err)
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()

	dur := time.Duration(secs) * time.Second
	deadline := time.After(dur)
	frames, bytes := 0, 0
	start := time.Now()
loop:
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				break loop
			}
			f.Write(b)
			frames++
			bytes += len(b)
		case <-deadline:
			break loop
		}
	}
	elapsed := time.Since(start).Seconds()
	cancel()
	time.Sleep(300 * time.Millisecond)
	t.Logf("dumped %d frames, %d bytes, %.1f fps over %.1fs → %s",
		frames, bytes, float64(frames)/elapsed, elapsed, out)
	if frames == 0 {
		t.Fatal("no frames captured")
	}
}
