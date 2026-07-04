//go:build linux

package capture

// Захват системного звука на Linux: PulseAudio (или PipeWire-pulse) monitor →
// libopus (ffmpeg) → ogg → отдельные Opus-пакеты в канал. НЕ переносимо на
// «Linux вообще» — жёстко завязано на PulseAudio-совместимый сервер и его
// @DEFAULT_MONITOR@. Видео-часть — независимые процессы (см. linux_common.go).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AudioAvailable — доступен ли системный звук: PulseAudio (или PipeWire-pulse) и
// ffmpeg. Проверяем PULSE_SERVER и стандартный per-user сокет.
func AudioAvailable() bool {
	if FFmpegPath() == "" {
		return false
	}
	if os.Getenv("PULSE_SERVER") != "" {
		return true
	}
	sock := filepath.Join(fmt.Sprintf("/run/user/%d", os.Getuid()), "pulse", "native")
	_, err := os.Stat(sock)
	return err == nil
}

// startAudioProc запускает аудио-ffmpeg (PulseAudio → Opus) и возвращает канал
// Opus-пакетов. @DEFAULT_MONITOR@ = монитор дефолтного sink (СИСТЕМНЫЙ вывод), а
// не микрофон.
func startAudioProc(ctx context.Context) (chan []byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		// Малый буфер захвата PulseAudio (низкая латентность, без «наплыва» пакетов).
		"-fragment_size", "1920", // 48000*2ch*2B*20мс = 3840 байт кадра; фрагмент ~20мс
		"-f", "pulse", "-i", "@DEFAULT_MONITOR@",
		// Ровно 48к/стерео — совпадает с RTP-часами Opus у зрителя. application=audio
		// (не lowdelay: тот CELT-only, звучит «булькающе/водянисто» на музыке/системном
		// звуке). frame_duration=20 → один Opus-кадр = один RTP-пакет = 20 мс.
		"-ac", "2", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "128k", "-vbr", "on",
		"-application", "audio", "-frame_duration", "20",
		"-page_duration", "20000", "-flush_packets", "1",
		"-f", "ogg", "pipe:1",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	log.Printf("capture: ffmpeg audio %s", strings.Join(args, " "))
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
	out := make(chan []byte, 16)
	go func() {
		defer waitKill(cmd, "audio")
		readOggOpus(ctx, stdout, out)
	}()
	return out, nil
}

// readOggOpus читает ogg-поток и шлёт в канал ОТДЕЛЬНЫЕ Opus-пакеты (по 20 мс).
// КРИТИЧНО: страница ogg может нести НЕСКОЛЬКО пакетов (ffmpeg иногда пакует 2).
// Если слать страницу целиком как один RTP-сэмпл — склейка невалидна для декодера
// («бульк») и RTP-метка отстаёт от реального звука (20 мс меток на 40 мс аудио) →
// накопительный рассинхрон, браузер то ускоряет, то тормозит. Поэтому парсим
// lacing-таблицу страницы и режем на пакеты — как на маке (libopus отдаёт по
// одному пакету). Заголовки OpusHead/OpusTags пропускаем.
func readOggOpus(ctx context.Context, r io.Reader, out chan []byte) {
	defer close(out)
	br := bufio.NewReader(r)
	var cont []byte // пакет, продолжающийся с прошлой страницы (lacing 255 на конце)
	// Диагностика прихода: пакеты и разрывы >40 мс (норма — ровно 20 мс между
	// пакетами), сводка раз в ~10 с; ~500 пакетов/10с = захват здоров.
	var n, gaps int
	var maxGap time.Duration
	last := time.Now()
	statT := last
	for {
		pkts, err := parseOggPage(br)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("audio: ogg read: %v", err)
			}
			return
		}
		for i, p := range pkts.packets {
			if len(cont) > 0 {
				p = append(cont, p...)
				cont = nil
			}
			if i == len(pkts.packets)-1 && pkts.lastPartial {
				cont = append([]byte(nil), p...) // не завершён — доклеим со след. страницы
				break
			}
			if len(p) >= 8 && (string(p[:8]) == "OpusHead" || string(p[:8]) == "OpusTags") {
				continue
			}
			now := time.Now()
			if d := now.Sub(last); n > 0 {
				if d > 40*time.Millisecond {
					gaps++
				}
				if d > maxGap {
					maxGap = d
				}
			}
			last = now
			n++
			if now.Sub(statT) >= 10*time.Second {
				log.Printf("audio: arrival n=%d gaps>40ms=%d max=%.0fms (10s)", n, gaps, maxGap.Seconds()*1000)
				n, gaps, maxGap = 0, 0, 0
				statT = now
			}
			select {
			case out <- p:
			case <-ctx.Done():
				return
			}
		}
	}
}

// oggPage — пакеты одной ogg-страницы; lastPartial=true, если последний пакет
// не завершён (lacing 255 в конце) и продолжается на следующей странице.
type oggPage struct {
	packets     [][]byte
	lastPartial bool
}

// parseOggPage читает одну страницу ogg (RFC 3533): заголовок 27 байт "OggS...",
// таблица lacing-значений (255 = пакет продолжается), payload. Режет payload на
// пакеты по lacing.
func parseOggPage(br *bufio.Reader) (oggPage, error) {
	var pg oggPage
	var hdr [27]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return pg, err
	}
	if string(hdr[:4]) != "OggS" {
		return pg, fmt.Errorf("bad ogg capture pattern")
	}
	nseg := int(hdr[26])
	lacing := make([]byte, nseg)
	if _, err := io.ReadFull(br, lacing); err != nil {
		return pg, err
	}
	total := 0
	for _, l := range lacing {
		total += int(l)
	}
	payload := make([]byte, total)
	if _, err := io.ReadFull(br, payload); err != nil {
		return pg, err
	}
	off, start := 0, 0
	for _, l := range lacing {
		off += int(l)
		if l < 255 { // пакет завершён
			pg.packets = append(pg.packets, payload[start:off])
			start = off
		}
	}
	if start < total { // хвост без терминатора — продолжится на след. странице
		pg.packets = append(pg.packets, payload[start:])
		pg.lastPartial = true
	}
	return pg, nil
}
