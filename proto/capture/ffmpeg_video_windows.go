//go:build windows

package capture

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// ffmpegVideoEnc — аппаратный H264 на Windows через ffmpeg-подпроцесс.
//
// Почему так, а не системный Media Foundation: релиз Windows кросс-компилируется
// из Linux с CGO_ENABLED=0, поэтому нативную обёртку к системному энкодеру писать
// нельзя — только чистые syscall/COM. Синхронный софт-MFT дохнет под нагрузкой, а
// аппаратный (AMD AMF) отдаёт кадры только через async IMFAsyncCallback, что на
// чистом Go-без-cgo нереализуемо. ffmpeg уже собран с h264_amf/nvenc/qsv, дёргает
// тот же GPU-энкодер, но как отдельный процесс — ни cgo, ни async-COM не нужно.
// Ровно тот же приём, что на Linux (VAAPI) и для звука на самой винде (WASAPI→Opus).
//
// Формат: NV12 (тот же, что готовит WGC-конвертер) пишем в stdin ffmpeg, читаем
// Annex-B H264 из stdout в общий канал кадров. Ключевые кадры — по фиксированному
// GOP (реакции на PLI нет: pipe так не умеет; при 2с GOP присоединение зрителя
// ждёт максимум пару секунд, что терпимо и лучше неработающего аппаратного пути).
type ffmpegVideoEnc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	name  string        // выбранный энкодер (h264_amf/…)
	done  chan struct{} // закрывается, когда горутина-читатель stdout вышла
}

var (
	winHWEncOnce sync.Once
	winHWEncName string
)

// pickWindowsH264Encoder один раз пробует аппаратные энкодеры (в порядке
// AMD→Intel→NVIDIA) и кэширует первый рабочий; при отсутствии — libx264 (софт).
func pickWindowsH264Encoder(ff string) string {
	winHWEncOnce.Do(func() {
		for _, enc := range []string{"h264_amf", "h264_qsv", "h264_nvenc"} {
			if probeFFmpegEncoder(ff, enc) {
				winHWEncName = enc
				return
			}
		}
		winHWEncName = "libx264"
	})
	return winHWEncName
}

// probeFFmpegEncoder гоняет кодек на одном чёрном кадре — быстрый способ понять,
// поднимется ли энкодер на этой машине (нет GPU/драйвера → ненулевой exit).
func probeFFmpegEncoder(ff, enc string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ff,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=256x256:r=30",
		"-frames:v", "1", "-c:v", enc, "-f", "null", "-")
	ok := cmd.Run() == nil
	log.Printf("ffmpeg: probe %s → %v", enc, ok)
	return ok
}

// encoderArgs — CBR + VBV (буфер ~0.5с) + без B-кадров + low-latency пресет под
// конкретный энкодер. CBR/VBV гасит всплески на движении (перетаскивание окна),
// из-за которых latency на WAN подскакивала до секунд.
func encoderArgs(enc string, kbps, gop int) []string {
	br := strconv.Itoa(kbps) + "k"
	buf := strconv.Itoa(kbps/2) + "k"
	common := func(codec string, pre ...string) []string {
		a := append([]string{"-c:v", codec}, pre...)
		return append(a, "-b:v", br, "-maxrate", br, "-bufsize", buf,
			"-g", strconv.Itoa(gop), "-bf", "0", "-pix_fmt", "nv12")
	}
	switch enc {
	case "h264_amf":
		return common("h264_amf", "-usage", "lowlatency", "-rc", "cbr", "-quality", "speed")
	case "h264_nvenc":
		return common("h264_nvenc", "-preset", "p1", "-tune", "ll", "-rc", "cbr")
	case "h264_qsv":
		return common("h264_qsv", "-preset", "veryfast", "-low_delay_brc", "1")
	default:
		return common("libx264", "-preset", "ultrafast", "-tune", "zerolatency")
	}
}

// newFFmpegVideoEnc поднимает ffmpeg (rawvideo NV12 в stdin → Annex-B H264 в
// stdout) и стартует читателя stdout в общий канал кадров.
func newFFmpegVideoEnc(ctx context.Context, frames chan []byte, w, h, fps, kbps int, dropLate bool) (*ffmpegVideoEnc, error) {
	ff := FFmpegPath()
	if ff == "" {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	enc := pickWindowsH264Encoder(ff)
	gop := fps * 2

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-f", "rawvideo", "-pix_fmt", "nv12",
		"-s", fmt.Sprintf("%dx%d", w, h), "-r", strconv.Itoa(fps),
		"-i", "pipe:0",
	}
	args = append(args, encoderArgs(enc, kbps, gop)...)
	args = append(args, "-f", "h264", "pipe:1")

	cmd := exec.CommandContext(ctx, ff, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go logStderr(stderr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		readH264(ctx, stdout, frames, dropLate)
	}()

	return &ffmpegVideoEnc{cmd: cmd, stdin: stdin, name: enc, done: done}, nil
}

// writeFrame отдаёт один NV12-кадр в stdin. Ошибка = ffmpeg умер → стрим мёртв.
func (f *ffmpegVideoEnc) writeFrame(nv12 []byte) error {
	_, err := f.stdin.Write(nv12)
	return err
}

// Close глушит ffmpeg и ДОЖИДАЕТСЯ выхода читателя stdout, прежде чем вернуться,
// — иначе гонка с close(frames) в run() (push в закрытый канал = паника).
func (f *ffmpegVideoEnc) Close() {
	if f.stdin != nil {
		f.stdin.Close()
	}
	if f.cmd != nil && f.cmd.Process != nil {
		f.cmd.Process.Kill()
	}
	<-f.done
	if f.cmd != nil {
		f.cmd.Wait()
	}
}
