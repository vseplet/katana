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

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
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

	wroteFirst bool // диагностика: залогировать первую запись в stdin
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
		"-fflags", "nobuffer",
		"-f", "rawvideo", "-pix_fmt", "nv12",
		"-s", fmt.Sprintf("%dx%d", w, h), "-r", strconv.Itoa(fps),
		"-i", "pipe:0",
	}
	args = append(args, encoderArgs(enc, kbps, gop)...)
	// -flush_packets 1: отдавать каждый AU в pipe немедленно, без буфера avio —
	// иначе ffmpeg копит вывод и первые кадры к зрителю не приходят (чёрный экран).
	args = append(args, "-flush_packets", "1", "-f", "h264", "pipe:1")

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
		readH264KeepHeaders(ctx, &countReader{r: stdout}, frames, dropLate)
	}()

	return &ffmpegVideoEnc{cmd: cmd, stdin: stdin, name: enc, done: done}, nil
}

// readH264KeepHeaders — как readH264, но гарантирует SPS/PPS перед каждым IDR.
//
// Захват (и ffmpeg) стартуют ДО подключения зрителей, поэтому любой зритель
// заходит в середину потока: без SPS/PPS в ключевом кадре его декодер не заведётся
// (пакеты идут, картинки нет — чёрный экран). libx264 повторяет заголовки при
// каждом IDR сам, а h264_amf их шлёт один раз в начале — до того, как кто-либо
// подключился. Кешируем последние SPS/PPS и подставляем их перед IDR, где их нет.
// Если энкодер и так их включает — ветка подстановки не срабатывает (no-op).
func readH264KeepHeaders(ctx context.Context, in io.Reader, frames chan []byte, dropLate bool) {
	reader, err := h264reader.NewReader(in)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("capture: h264 reader: %v", err)
		}
		return
	}
	sc := []byte{0x00, 0x00, 0x00, 0x01}
	var au []byte
	var sps, pps []byte // закешированы в Annex-B (со start code)
	var auHasParams bool // в текущем AU уже встретились SPS/PPS
	for {
		nal, err := reader.NextNAL()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("capture: read nal: %v", err)
			}
			return
		}
		switch nal.UnitType {
		case h264reader.NalUnitTypeSPS:
			sps = append(append([]byte{}, sc...), nal.Data...)
			auHasParams = true
		case h264reader.NalUnitTypePPS:
			pps = append(append([]byte{}, sc...), nal.Data...)
			auHasParams = true
		}
		au = append(au, sc...)
		au = append(au, nal.Data...)

		isVCL := nal.UnitType == h264reader.NalUnitTypeCodedSliceNonIdr ||
			nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr
		if !isVCL {
			continue
		}
		out := au
		if nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr && !auHasParams && sps != nil && pps != nil {
			out = append(append(append([]byte{}, sps...), pps...), au...)
		}
		if !pushFrame(ctx, frames, out, dropLate) {
			return
		}
		au = nil
		auHasParams = false
	}
}

// countReader логирует первый байт вывода ffmpeg (диагностика: доходит ли
// закодированный поток из stdout до читателя) и молчит дальше.
type countReader struct {
	r     io.Reader
	first bool
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && !c.first {
		c.first = true
		log.Printf("ffmpeg: first H264 output from stdout (%d bytes)", n)
	}
	return n, err
}

// writeFrame отдаёт один NV12-кадр в stdin. Ошибка = ffmpeg умер → стрим мёртв.
func (f *ffmpegVideoEnc) writeFrame(nv12 []byte) error {
	n, err := f.stdin.Write(nv12)
	if !f.wroteFirst {
		f.wroteFirst = true
		log.Printf("ffmpeg: first NV12 frame written to stdin (%d bytes, err=%v)", n, err)
	}
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
