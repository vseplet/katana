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
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/vseplet/katana/proto/capture"
	"github.com/vseplet/katana/proto/permissions"
	"golang.org/x/term"
)

// version вшивается при релизной сборке: -ldflags "-X main.version=v0.1.2".
// Установочный скрипт сравнивает её с последним релизом и качает только при отличии.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
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

	if *showVersion {
		fmt.Println(version)
		return
	}

	sessionID := *session
	if sessionID == "" {
		sessionID = *id
	}
	if sessionID == "" {
		log.Fatalf("--session=<uuid> is required (broker session code; created on the katana-saas site)")
	}

	// Логи уводим в ~/.katana/<session>.log, чтобы терминал занял TUI. В fallback
	// (не TTY: фон/пайп) дублируем в stdout, иначе вывода не будет вообще.
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	logPath := sessionLogPath(sessionID)
	if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		defer lf.Close()
		if tty {
			log.SetOutput(lf)
		} else {
			log.SetOutput(io.MultiWriter(os.Stdout, lf))
		}
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
			log.Printf("permissions: screen recording granted")
		} else {
			log.Printf("permissions: no screen recording access — grant it in System Settings → Screen Recording and restart the terminal")
		}
	}

	// H264 (по умолчанию) кодируется нативно через VideoToolbox — ffmpeg не нужен.
	// ffmpeg требуется только для VP8 и --test; если его нет — лишь предупреждаем.
	if ff := capture.FFmpegPath(); ff != "" {
		log.Printf("ffmpeg: %s", ff)
	} else {
		log.Printf("ffmpeg not found (ok for H264; needed only for VP8 / --test) — install via brew or ~/.katana/bin/ffmpeg")
	}

	log.Printf("katana host: session %s via %s", sessionID, *broker)
	run := func() { runBrokerHost(ctx, *broker, sessionID, enc, opts) }
	if tty {
		runHostUI(sessionID, viewerURL(*broker, sessionID), logPath, stop, run) // живой статус + QR
	} else {
		run() // не TTY (фон/пайп) — обычный лог-вывод
	}
	log.Printf("stopped")
}

// sessionLogPath — файл логов сессии рядом с бинарём: ~/.katana/<session>.log.
func sessionLogPath(session string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".katana", session+".log")
}
