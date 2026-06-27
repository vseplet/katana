// Command proto — PoC WebRTC-стриминга экрана macOS в браузер.
//
// Запуск:
//
//	go run .                 # дефолты: :8080, screen 0, 1280px, 30fps
//	go run . --screen 1      # другой индекс экрана (см. -list_devices)
//
// Узнать индекс экрана:
//
//	ffmpeg -f avfoundation -list_devices true -i ""
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/vseplet/katana/proto/capture"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "адрес HTTP-сервера")
	// Индекс плавает между машинами; на этой "Capture screen 0" == 3.
	// Проверять перед запуском: ffmpeg -f avfoundation -list_devices true -i ""
	screen := flag.Int("screen", 3, "индекс экрана avfoundation (см. -list_devices)")
	width := flag.Int("width", 1280, "целевая ширина в пикселях (0 = нативное)")
	fps := flag.Int("fps", 30, "частота кадров")
	bitrate := flag.String("bitrate", "3M", "целевой битрейт видео")
	codec := flag.String("codec", "vp8", "кодек по умолчанию: vp8 | h264 (клиент может переопределить)")
	test := flag.Bool("test", false, "использовать синтетический testsrc вместо экрана (отладка без TCC)")
	flag.Parse()

	opts := capture.Options{
		ScreenIndex: *screen,
		Codec:       capture.Codec(*codec),
		Width:       *width,
		FPS:         *fps,
		Bitrate:     *bitrate,
		TestSource:  *test,
	}
	enc := capture.NewFFmpegDarwin()

	// Статика из web/ (через embed — бинарь самодостаточен).
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed sub: %v", err)
	}

	// Корневой контекст: отменяется по SIGINT/SIGTERM. Все захваты
	// наследуются от него, поэтому при остановке сервера каждый ffmpeg
	// гарантированно убивается (CommandContext шлёт Kill даже зависшему).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/ws", signalingHandler(ctx, enc, opts))

	srv := &http.Server{Addr: *addr, Handler: mux}

	go func() {
		log.Printf("listening on http://localhost%s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
