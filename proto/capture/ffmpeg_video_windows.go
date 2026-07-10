//go:build windows

package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
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

	// Развязка захват↔кодирование (ключ к отсутствию залипаний на Windows):
	// поток захвата НЕ пишет в stdin ffmpeg напрямую — иначе он блокируется, пока
	// ffmpeg не вычитает 3МБ-кадр из ~64КБ-трубы, и на тяжёлом IDR встаёт (фриз).
	// Вместо этого кадр копируется и кладётся в очередь, а отдельная горутина
	// writeLoop сливает её в stdin в своём темпе. При переполнении дропаем самый
	// старый кадр — захват идёт ровно 60fps и никогда не ждёт энкодер, как на mac/linux.
	in    chan []byte   // очередь NV12-кадров к писателю
	free  chan []byte   // пул переиспользуемых буферов (меньше GC)
	stop  chan struct{} // сигнал остановки писателя
	wdone chan struct{} // закрывается, когда writeLoop вышел
	dead  int32         // atomic: ffmpeg умер / энкодер закрыт

	wroteFirst bool  // диагностика: залогировать первую постановку кадра в очередь
	nDropped   int64 // счётчик дропнутых кадров (лог раз в N)
	gotOutput  int32 // atomic: ffmpeg выдал хоть один байт H264 (энкодер жив)
}

// killStrayFFmpeg добивает осиротевшие ffmpeg от прошлых запусков katana. На Windows
// дочерний процесс переживает жёсткую смерть родителя и продолжает держать AMF-сессию
// AMD — из-за этого новый h264_amf падает на инициализации, и мы валимся в софт.
// Вызывать ДО старта наших ffmpeg (в самом начале захвата), иначе убьём свои же.
func killStrayFFmpeg() {
	out, err := exec.Command("taskkill", "/F", "/IM", "ffmpeg.exe").CombinedOutput()
	if err == nil {
		log.Printf("capture: подчистил осиротевшие ffmpeg перед стартом: %s", strings.TrimSpace(string(out)))
	}
}

// ffmpegEncoderCandidates — порядок кандидатов: аппаратные (если сборка ffmpeg их
// содержит) → libx264. Наличие проверяем по СПИСКУ `ffmpeg -encoders`, который НЕ
// открывает GPU-сессию — в отличие от старой пробы-через-кодирование, которая сама
// занимала AMF-сессию и конфликтовала с реальным запуском. Реальную работоспособность
// каждого кандидата проверяет health-check уже на боевом процессе (waitHealthy).
func ffmpegEncoderCandidates(ff string) []string {
	out, _ := exec.Command(ff, "-hide_banner", "-encoders").Output()
	list := string(out)
	var c []string
	for _, enc := range []string{"h264_amf", "h264_qsv", "h264_nvenc"} {
		if strings.Contains(list, enc) {
			c = append(c, enc)
		}
	}
	return append(c, "libx264") // софт-фолбэк — гарантированно есть и не занимает GPU
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
		a := []string{"-usage", "lowlatency", "-rc", "cbr", "-quality", "speed"}
		// enforce_hrd строго держит CBR на ключевом кадре (кандидат на лечение
		// периодического всплеска). НЕ включаем по умолчанию: на части сборок
		// ffmpeg/драйверов AMF эти опции роняют энкодер — держим за env-флагом,
		// чтобы дефолт всегда стартовал.
		if os.Getenv("KATANA_AMF_HRD") == "1" {
			a = append(a, "-enforce_hrd", "1", "-filler_data", "0", "-frame_skipping", "0")
		}
		return common("h264_amf", a...)
	case "h264_nvenc":
		return common("h264_nvenc", "-preset", "p1", "-tune", "ll", "-rc", "cbr")
	case "h264_qsv":
		return common("h264_qsv", "-preset", "veryfast", "-low_delay_brc", "1")
	default:
		// libx264 — софт-фолбэк. В 1080p60 он не тянет реалтайм (оверлоад), поэтому
		// даунскейлим до 720p: программный H264 в 720p спокойно идёт в такт.
		return common("libx264", "-vf", "scale=-2:720", "-preset", "ultrafast", "-tune", "zerolatency")
	}
}

// newFFmpegVideoEnc выбирает рабочий энкодер с фолбэком: перебирает кандидатов
// (аппаратный → libx264), для каждого поднимает БОЕВОЙ ffmpeg и health-check'ом
// проверяет, что он реально выдаёт поток. Первый живой и возвращается.
//
// Почему health-check на боевом процессе, а не отдельной пробой: h264_amf на
// драйверах AMD то поднимается, то нет (сессия занята сиротой/не отпущена драйвером),
// а любая ВТОРАЯ h264_amf-сессия (проба/валидация) сама провоцирует конфликт. Здесь
// AMF-сессия открывается РОВНО ОДИН раз — если она не завелась, кадры от неё не
// пойдут, мы её глушим и падаем на libx264. Чёрного экрана быть не может: libx264
// (софт) поднимается всегда.
func newFFmpegVideoEnc(ctx context.Context, frames chan []byte, w, h, fps, kbps int, dropLate bool) (*ffmpegVideoEnc, error) {
	ff := FFmpegPath()
	if ff == "" {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	// GOP (интервал ключевых кадров) настраиваем через env без пересборки: короче —
	// быстрее заход зрителя, длиннее — реже IDR-всплески. По умолчанию 2с.
	gopSec := 2
	if v := os.Getenv("KATANA_GOP_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 60 {
			gopSec = n
		}
	}
	gop := fps * gopSec

	var lastErr error
	for _, enc := range ffmpegEncoderCandidates(ff) {
		e, err := startFFmpegEnc(ctx, ff, frames, enc, w, h, fps, kbps, gop, gopSec, dropLate)
		if err != nil {
			lastErr = err
			log.Printf("ffmpeg: не удалось запустить %s: %v — беру следующий", enc, err)
			continue
		}
		if e.waitHealthy(w, h) {
			return e, nil
		}
		log.Printf("ffmpeg: %s не выдал поток (сессия занята/краш инициализации) — фолбэк", enc)
		e.Close()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no working ffmpeg encoder")
	}
	return nil, lastErr
}

// startFFmpegEnc поднимает боевой ffmpeg на заданном энкодере и стартует горутины
// читателя stdout и писателя stdin. Готовность энкодера НЕ проверяет — это waitHealthy.
func startFFmpegEnc(ctx context.Context, ff string, frames chan []byte, enc string, w, h, fps, kbps, gop, gopSec int, dropLate bool) (*ffmpegVideoEnc, error) {
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
	log.Printf("ffmpeg: video %s %dx%d @ %dfps %dkbps gop=%d(%ds)", enc, w, h, fps, kbps, gop, gopSec)
	go logStderr(stderr)

	e := &ffmpegVideoEnc{
		cmd:   cmd,
		stdin: stdin,
		name:  enc,
		done:  make(chan struct{}),
		in:    make(chan []byte, 4),
		free:  make(chan []byte, 6),
		stop:  make(chan struct{}),
		wdone: make(chan struct{}),
	}
	go func() {
		defer close(e.done)
		readH264KeepHeaders(ctx, &countReader{r: stdout, flag: &e.gotOutput}, frames, dropLate)
	}()
	go e.writeLoop()
	return e, nil
}

// waitHealthy скармливает энкодеру несколько чёрных кадров и ждёт ~2с, пока он выдаст
// первый байт H264 ИЛИ умрёт. true = энкодер жив (пошёл поток). Использует тот же
// боевой процесс — второй GPU-сессии не открывает. Чёрные кадры уходят в начало
// стрима (зритель обычно ещё не подключён) — безвредно.
func (e *ffmpegVideoEnc) waitHealthy(w, h int) bool {
	black := make([]byte, w*h*3/2) // NV12; нули = зелёный кадр, для проверки цвет неважен
	for i := 0; i < 8; i++ {
		if atomic.LoadInt32(&e.dead) != 0 {
			return false
		}
		_ = e.writeFrame(black)
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	for {
		if atomic.LoadInt32(&e.gotOutput) != 0 {
			return true
		}
		select {
		case <-e.done: // читатель вышел = процесс умер
			return atomic.LoadInt32(&e.gotOutput) != 0
		case <-deadline.C:
			return atomic.LoadInt32(&e.gotOutput) != 0
		case <-tick.C:
		}
	}
}

// writeLoop сливает очередь NV12-кадров в stdin ffmpeg. Блокировка тут (пока ffmpeg
// не вычитает кадр) НЕ трогает поток захвата — в этом весь смысл развязки.
func (e *ffmpegVideoEnc) writeLoop() {
	defer close(e.wdone)
	for {
		select {
		case <-e.stop:
			return
		case b := <-e.in:
			if _, err := e.stdin.Write(b); err != nil {
				atomic.StoreInt32(&e.dead, 1)
				select {
				case <-e.stop: // штатное закрытие — не шумим
				default:
					log.Printf("capture: ffmpeg video write: %v", err)
				}
				return
			}
			select {
			case e.free <- b: // вернуть буфер в пул
			default:
			}
		}
	}
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

// countReader на первом же байте вывода ffmpeg взводит флаг gotOutput (его читает
// waitHealthy, чтобы понять, что энкодер реально ожил) и логирует факт. Дальше молчит.
type countReader struct {
	r    io.Reader
	flag *int32
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && atomic.CompareAndSwapInt32(c.flag, 0, 1) {
		log.Printf("ffmpeg: first H264 output from stdout (%d bytes)", n)
	}
	return n, err
}

// writeFrame НЕ блокирует поток захвата: копирует кадр в буфер из пула и кладёт в
// очередь; при переполнении дропает самый старый (dropLate). Ошибка = ffmpeg умер.
func (f *ffmpegVideoEnc) writeFrame(nv12 []byte) error {
	if atomic.LoadInt32(&f.dead) != 0 {
		return errors.New("ffmpeg writer stopped")
	}
	// Копия обязательна: поток захвата переиспользует свой nv12-буфер сразу после
	// возврата, а писатель заберёт кадр позже — иначе кадр будет затёрт на лету.
	var b []byte
	select {
	case b = <-f.free:
		b = append(b[:0], nv12...)
	default:
		b = append(make([]byte, 0, len(nv12)), nv12...)
	}
	if !f.wroteFirst {
		f.wroteFirst = true
		log.Printf("ffmpeg: first NV12 frame queued (%d bytes)", len(b))
	}
	for {
		select {
		case f.in <- b:
			return nil
		default:
			// Очередь полна — ffmpeg отстаёт (напр. кодирует IDR). Выкидываем самый
			// старый кадр и пробуем снова: захват не ждёт, теряем максимум кадр-два.
			select {
			case old := <-f.in:
				if n := atomic.AddInt64(&f.nDropped, 1); n%60 == 1 {
					log.Printf("ffmpeg: video queue full, dropped %d frame(s) total", n)
				}
				select {
				case f.free <- old:
				default:
				}
			default:
			}
		}
	}
}

// Close останавливает писателя и читателя и глушит ffmpeg. Дожидается ОБЕИХ горутин
// до возврата — иначе гонка с close(frames) в run() (push в закрытый канал = паника).
func (f *ffmpegVideoEnc) Close() {
	atomic.StoreInt32(&f.dead, 1) // writeFrame перестаёт принимать кадры
	close(f.stop)                 // сигнал writeLoop
	if f.stdin != nil {
		f.stdin.Close() // разблокирует зависшую в Write горутину writeLoop
	}
	if f.cmd != nil && f.cmd.Process != nil {
		f.cmd.Process.Kill()
	}
	<-f.wdone // писатель вышел
	<-f.done  // читатель вышел
	if f.cmd != nil {
		f.cmd.Wait()
	}
}
