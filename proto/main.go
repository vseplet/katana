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
	"os"
	"os/signal"
	"syscall"

	"github.com/vseplet/katana/proto/capture"
	"github.com/vseplet/katana/proto/permissions"
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
	dev := flag.Bool("dev", false, "отдавать web/ с диска (правки фронта без пересборки бинаря)")
	flag.Parse()

	opts := capture.Options{
		SourceKind:  "display", // по умолчанию весь дисплей через ScreenCaptureKit
		Cursor:      true,      // показывать курсор хоста (выключается при управлении)
		ScreenIndex: *screen,
		Codec:       capture.Codec(*codec),
		Width:       *width,
		FPS:         *fps,
		Bitrate:     *bitrate,
		TestSource:  *test,
	}
	enc := capture.NewFFmpegDarwin()

	// Статика из web/. По умолчанию — из embed (бинарь самодостаточен).
	// С --dev отдаём с диска (./web) без кеша, чтобы править фронт без пересборки.
	var static fs.FS
	if *dev {
		static = os.DirFS("web")
		log.Printf("dev: web/ отдаётся с диска (без кеша)")
	} else {
		sub, err := fs.Sub(webFS, "web")
		if err != nil {
			log.Fatalf("embed sub: %v", err)
		}
		static = sub
	}
	fileHandler := http.FileServer(http.FS(static))
	if *dev {
		fileHandler = noCache(fileHandler)
	}

	// Корневой контекст: отменяется по SIGINT/SIGTERM. Все захваты
	// наследуются от него, поэтому при остановке сервера каждый ffmpeg
	// гарантированно убивается (CommandContext шлёт Kill даже зависшему).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/", fileHandler)
	mux.HandleFunc("/ws", signalingHandler(ctx, enc, opts))
	registerPermissionRoutes(mux)
	registerDisplayRoutes(mux, opts.ScreenIndex)

	// Запрашиваем запись экрана при старте (показывает системный диалог, если
	// решение ещё не принято). Без разрешения захват вернёт чёрный кадр (§7 ТЗ).
	// ВАЖНО: для bare-CLI macOS привязывает доступ к «ответственному» приложению —
	// терминалу (Ghostty/Terminal/iTerm), а не к самому katana (см. README).
	if !*test {
		if permissions.RequestScreenCapture() {
			log.Printf("permissions: запись экрана разрешена")
		} else {
			log.Printf("permissions: нет доступа к записи экрана — выдай в диалоге/System Settings и перезапусти терминал")
		}
	}

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

// noCache отключает кеширование статики (для --dev: правки фронта видны без
// хард-релоада и без пересборки бинаря).
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		h.ServeHTTP(w, r)
	})
}
