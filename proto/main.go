// Command katana host — захват экрана/звука macOS + ввод + общий терминал,
// поверх WebRTC. Хост подключается исходящим WS к рандеву-брокеру (katana-saas)
// по коду сессии; зритель находит его там же. Локального веб-сервера нет —
// клиент отдаёт SaaS.
//
// Запуск:
//
//	go run . --id=<uuid>                       # через дефолтный брокер
//	go run . --id=<uuid> --broker=wss://…/rtc  # свой брокер/туннель
//
// Индекс экрана: ffmpeg -f avfoundation -list_devices true -i ""
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/vseplet/katana/proto/capture"
	"github.com/vseplet/katana/proto/permissions"
)

func main() {
	session := flag.String("session", "", "код сессии (UUID) рандеву-брокера — обязателен")
	id := flag.String("id", "", "алиас --session")
	broker := flag.String("broker", "wss://katana.vseplet.deno.net/rtc", "URL рандеву-брокера (эндпоинт /rtc)")
	screen := flag.Int("screen", 3, "индекс экрана avfoundation (см. -list_devices)")
	width := flag.Int("width", 1280, "целевая ширина в пикселях (0 = нативное)")
	fps := flag.Int("fps", 30, "частота кадров")
	bitrate := flag.String("bitrate", "3M", "целевой битрейт видео")
	codec := flag.String("codec", "h264", "кодек: h264 (VideoToolbox) | vp8")
	audio := flag.Bool("audio", false, "передавать звук (SCK → Opus)")
	test := flag.Bool("test", false, "синтетический testsrc вместо экрана (отладка без TCC)")
	flag.Parse()

	sessionID := *session
	if sessionID == "" {
		sessionID = *id
	}
	if sessionID == "" {
		log.Fatalf("нужен --session=<uuid> (код сессии брокера; создаётся на сайте katana-saas)")
	}

	opts := capture.Options{
		SourceKind:  "display", // по умолчанию весь дисплей через ScreenCaptureKit
		Cursor:      true,      // курсор хоста (выключается при управлении мышью)
		ScreenIndex: *screen,
		Codec:       capture.Codec(*codec),
		Width:       *width,
		FPS:         *fps,
		Bitrate:     *bitrate,
		Audio:       *audio,
		TestSource:  *test,
	}
	enc := capture.NewFFmpegDarwin()

	// Корневой контекст: отмена по SIGINT/SIGTERM останавливает захват/ffmpeg.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// TCC: запись экрана. Разрешение привязывается к терминалу, из которого
	// запущено (не к самому бинарю).
	if !*test {
		if permissions.RequestScreenCapture() {
			log.Printf("permissions: запись экрана разрешена")
		} else {
			log.Printf("permissions: нет доступа к записи экрана — выдай в System Settings → Screen Recording и перезапусти терминал")
		}
	}

	// ffmpeg обязателен (энкодер): ~/.katana/bin/ffmpeg → PATH → иначе падаем сразу.
	if ff := capture.FFmpegPath(); ff == "" {
		log.Fatalf("ffmpeg не найден: положи бинарник в ~/.katana/bin/ffmpeg или установи ffmpeg (brew install ffmpeg)")
	} else {
		log.Printf("ffmpeg: %s", ff)
	}

	log.Printf("katana host: session %s через %s", sessionID, *broker)
	runBrokerHost(ctx, *broker, sessionID, enc, opts)
	log.Printf("остановлено")
}
