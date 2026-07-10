//go:build windows

package capture

// Системный звук на Windows: WASAPI loopback (нативно, COM) снимает микс дефолтного
// устройства вывода → сырой PCM → ffmpeg (libopus) → Ogg → отдельные Opus-пакеты в
// канал (тот же контракт, что mac/Linux). Opus-энкодера нативно на Windows нет
// (MFT умеет AAC, не Opus), поэтому кодирование — через ffmpeg, как и на других
// платформах; нужен ffmpeg в ~/.katana/bin или PATH (иначе AudioAvailable=false).
//
// WASAPI loopback НЕ отдаёт данные во время полной тишины (устройство вывода
// простаивает), поэтому паузы добиваем нулями по стенным часам — иначе ffmpeg
// склеит звук без пауз и он «убежит»/рассинхронится.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"strconv"
	"time"
	"unsafe"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

var (
	procCoInitializeEx = ole32.NewProc("CoInitializeEx")
	procCoUninitialize = ole32.NewProc("CoUninitialize")
)

var (
	clsidMMDeviceEnumerator = mustGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidIMMDeviceEnumerator  = mustGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidIAudioClient         = mustGUID("{1CB9AD4C-DBFA-4C32-B178-C2F568A703B2}")
	iidIAudioCaptureClient  = mustGUID("{C8ADBD64-E71E-48A0-A4DE-185C395CD317}")
)

const (
	coinitMultithreaded                 = 0x0
	clsctxAll                           = 0x17
	audclntShareModeShared              = 0
	audclntStreamflagsLoopback          = 0x00020000
	audclntBufferflagsDataDiscontinuity = 0x1 // захват заметил разрыв (overrun/глитч)
	audclntBufferflagsSilent            = 0x2
	loopbackBufDur100ns                 = 2000000 // 200 мс буфер захвата (в 100нс). Крупный запас,
	// чтобы пауза GC/планировщика (в VM бывает 50-100мс) не переполнила кольцо и не
	// потеряла сэмплы (звук залипал раз в 2-3с). Латентность не растёт: вычитываем
	// всё каждые 10мс, буфер — только запас на overrun.

	// Индексы vtable (IUnknown-интерфейсы: методы с индекса 3).
	idxEnumGetDefaultEndpoint   = 4  // IMMDeviceEnumerator::GetDefaultAudioEndpoint
	idxDeviceActivate           = 3  // IMMDevice::Activate
	idxAudioClientInitialize    = 3  // IAudioClient::Initialize
	idxAudioClientGetMixFormat  = 8  // GetMixFormat
	idxAudioClientStart         = 10 // Start
	idxAudioClientStop          = 11 // Stop
	idxAudioClientGetService    = 14 // GetService
	idxCaptureGetBuffer         = 3  // IAudioCaptureClient::GetBuffer
	idxCaptureReleaseBuffer     = 4  // ReleaseBuffer
	idxCaptureGetNextPacketSize = 5  // GetNextPacketSize
)

// waveFormatEx — WAVEFORMATEX (18 байт, C-раскладка совпадает с натуральным
// выравниванием полей). За ним при wFormatTag==0xFFFE идёт хвост WAVEFORMATEXTENSIBLE.
type waveFormatEx struct {
	wFormatTag      uint16
	nChannels       uint16
	nSamplesPerSec  uint32
	nAvgBytesPerSec uint32
	nBlockAlign     uint16
	wBitsPerSample  uint16
	cbSize          uint16
}

// loopback держит WASAPI-клиент и параметры формата микса.
type loopback struct {
	client    uintptr // IAudioClient
	capture   uintptr // IAudioCaptureClient
	rate      int
	channels  int
	blockAlgn int    // байт на фрейм (все каналы)
	sampleFmt string // формат для ffmpeg -f (f32le/s16le/s32le)
}

// setupLoopback поднимает WASAPI loopback на дефолтном устройстве вывода. Вызывать
// на потоке, где уже сделан CoInitializeEx.
func setupLoopback() (*loopback, error) {
	hr, enum := procCallOut(procCoCreateInstance,
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)))
	if err := hrError(hr, "CoCreateInstance(MMDeviceEnumerator)"); err != nil {
		return nil, err
	}
	defer comRelease(enum)

	// GetDefaultAudioEndpoint(eRender=0, eConsole=0, **device).
	hr, device := comCallOut(enum, idxEnumGetDefaultEndpoint, 0, 0)
	if err := hrError(hr, "GetDefaultAudioEndpoint"); err != nil {
		return nil, err
	}
	defer comRelease(device)

	// Activate(IID_IAudioClient, CLSCTX_ALL, nil, **client).
	hr, client := comCallOut(device, idxDeviceActivate,
		uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0)
	if err := hrError(hr, "Activate(IAudioClient)"); err != nil {
		return nil, err
	}

	// Формат микса (обычно float32 48кГц стерео).
	hr, pfmt := comCallOut(client, idxAudioClientGetMixFormat)
	if err := hrError(hr, "GetMixFormat"); err != nil {
		comRelease(client)
		return nil, err
	}
	wf := (*waveFormatEx)(unsafe.Pointer(pfmt))
	rate := int(wf.nSamplesPerSec)
	channels := int(wf.nChannels)
	blockAlgn := int(wf.nBlockAlign)
	sampleFmt := ffmpegSampleFmt(wf, pfmt)

	// Initialize(SHARED, LOOPBACK, bufDur, 0, pFormat, nil). loopback + shared:
	// периодичность обязана быть 0, тактируем чтением сами.
	hr = comCall(client, idxAudioClientInitialize,
		audclntShareModeShared, audclntStreamflagsLoopback,
		uintptr(int64(loopbackBufDur100ns)), 0, pfmt, 0)
	procCoTaskMemFree.Call(pfmt) // формат больше не нужен
	if err := hrError(hr, "IAudioClient::Initialize"); err != nil {
		comRelease(client)
		return nil, err
	}

	hr, capClient := comCallOut(client, idxAudioClientGetService,
		uintptr(unsafe.Pointer(&iidIAudioCaptureClient)))
	if err := hrError(hr, "GetService(IAudioCaptureClient)"); err != nil {
		comRelease(client)
		return nil, err
	}

	if hr := comCall(client, idxAudioClientStart); hr != sOK {
		comRelease(capClient)
		comRelease(client)
		return nil, hrError(hr, "IAudioClient::Start")
	}

	log.Printf("capture: WASAPI loopback %dHz %dch %s (blockAlign=%d)", rate, channels, sampleFmt, blockAlgn)
	return &loopback{
		client: client, capture: capClient,
		rate: rate, channels: channels, blockAlgn: blockAlgn, sampleFmt: sampleFmt,
	}, nil
}

// ffmpegSampleFmt определяет формат сэмплов для ffmpeg -f по WAVEFORMATEX(EXTENSIBLE).
func ffmpegSampleFmt(wf *waveFormatEx, pfmt uintptr) string {
	const (
		wfIEEEFloat  = 0x0003
		wfPCM        = 0x0001
		wfExtensible = 0xFFFE
	)
	tag := uint32(wf.wFormatTag)
	if wf.wFormatTag == wfExtensible && wf.cbSize >= 22 {
		// SubFormat GUID начинается сразу после WAVEFORMATEX(18) + 6 байт хвоста;
		// его первые 4 байта (Data1) = формат-тег.
		tag = *(*uint32)(unsafe.Pointer(pfmt + 24))
	}
	switch {
	case tag == wfIEEEFloat && wf.wBitsPerSample == 32:
		return "f32le"
	case tag == wfPCM && wf.wBitsPerSample == 16:
		return "s16le"
	case tag == wfPCM && wf.wBitsPerSample == 32:
		return "s32le"
	default:
		return "f32le" // разумный дефолт для shared-режима WASAPI
	}
}

func (l *loopback) close() {
	if l.client != 0 {
		comCall(l.client, idxAudioClientStop)
	}
	if l.capture != 0 {
		comRelease(l.capture)
	}
	if l.client != 0 {
		comRelease(l.client)
	}
}

// pump снимает PCM и пишет в w (stdin ffmpeg) КАК ЕСТЬ, без собственного тактования
// и без ресемпла — так же, как mac отдаёт SCK-звук прямо в ffmpeg. WASAPI shared
// loopback уже отдаёт кадры в реальном времени тактовой аудиоустройства; на SILENT-
// флаг пишем нули такого же размера (тишина внутри активного потока). Свой ручной
// пейсинг и ffmpeg-aresample давали дрейф/щелчки/задержку — убраны. Блокирует до
// ctx.Done / ошибки записи.
func (l *loopback) pump(ctx context.Context, w io.Writer) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	zeros := make([]byte, 4096*l.blockAlgn) // буфер для «активной тишины» (SILENT-флаг)
	// Боксы out-параметров COM аллоцируем ОДИН раз и переиспользуем между итерациями.
	// new() на каждый пакет каждые 10мс подливал мусор в GC, чьи паузы приводили к
	// периодическим overrun'ам захвата (звук залипал раз в 2-3с). Куча-боксы всё так
	// же переживают перемещения Go-стека.
	pf := new(uint32)
	pData := new(uintptr)
	pFrames := new(uint32)
	pFlags := new(uint32)
	pDev := new(uint64)
	pQPC := new(uint64)

	var discont int     // счётчик overrun'ов (DATA_DISCONTINUITY) за окно
	statT := time.Now() // окно логирования разрывов

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if el := time.Since(statT); el >= 5*time.Second {
			if discont > 0 {
				log.Printf("audio: WASAPI overrun x%d за %.0fс (потеря сэмплов → залипание)", discont, el.Seconds())
			}
			discont, statT = 0, time.Now()
		}

		// Сливаем все готовые пакеты WASAPI за этот тик.
		for {
			if hr := comCall(l.capture, idxCaptureGetNextPacketSize, uintptr(unsafe.Pointer(pf))); hr != sOK {
				break
			}
			if *pf == 0 {
				break
			}
			hr := comCall(l.capture, idxCaptureGetBuffer,
				uintptr(unsafe.Pointer(pData)), uintptr(unsafe.Pointer(pFrames)),
				uintptr(unsafe.Pointer(pFlags)), uintptr(unsafe.Pointer(pDev)),
				uintptr(unsafe.Pointer(pQPC)))
			if hr != sOK {
				break
			}
			frames := *pFrames
			flags := *pFlags
			data := *pData
			if flags&audclntBufferflagsDataDiscontinuity != 0 {
				discont++
			}
			var werr error
			if frames > 0 {
				// SILENT-флаг: буфер валиден, но это тишина — пишем нули такого же размера.
				if flags&audclntBufferflagsSilent != 0 || data == 0 {
					need := int(frames) * l.blockAlgn
					for need > 0 {
						n := need
						if n > len(zeros) {
							n = len(zeros)
						}
						if _, werr = w.Write(zeros[:n]); werr != nil {
							break
						}
						need -= n
					}
				} else {
					buf := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(frames)*l.blockAlgn)
					_, werr = w.Write(buf)
				}
			}
			runtime.KeepAlive(pData)
			runtime.KeepAlive(pFrames)
			runtime.KeepAlive(pFlags)
			comCall(l.capture, idxCaptureReleaseBuffer, uintptr(frames))
			if werr != nil {
				return // ffmpeg закрылся
			}
		}
	}
}

// startOpusFromPCM запускает ffmpeg: сырой PCM (stdin) → libopus → Ogg (stdout).
// Возвращает stdin (куда pump пишет PCM) и канал Opus-пакетов (по 20 мс).
func startOpusFromPCM(ctx context.Context, sampleFmt string, rate, channels int) (io.WriteCloser, chan []byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", sampleFmt, "-ar", strconv.Itoa(rate), "-ac", strconv.Itoa(channels), "-i", "-",
		// Ровно 48к/стерео на выходе — совпадает с RTP-часами Opus у зрителя.
		// Поток держим непрерывным и real-time на стороне захвата (pump), поэтому
		// ffmpeg простой, как на mac/Linux — без ресемпла/пересинхронизации.
		"-ac", "2", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "128k", "-application", "lowdelay",
		"-page_duration", "20000", // одна opus-страница ≈ 20 мс — как на mac
		"-f", "ogg", "-",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	go logStderr(stderr)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	assignToKillJob(cmd) // умрёт вместе с katana даже при жёстком выходе

	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		reader, _, err := oggreader.NewWith(bufio.NewReader(stdout))
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("capture: ogg header: %v", err)
			}
			return
		}
		for {
			page, _, err := reader.ParseNextPage()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("audio: ogg read: %v", err)
				}
				return
			}
			select {
			case out <- page:
			case <-ctx.Done():
				return
			}
		}
	}()
	return stdin, out, nil
}

// startAudioWindows поднимает весь аудио-путь (WASAPI loopback → ffmpeg → Opus) на
// отдельном залоченном потоке с COM. Возвращает канал Opus-пакетов.
func startAudioWindows(ctx context.Context) (chan []byte, error) {
	if FFmpegPath() == "" {
		return nil, errNoFFmpeg
	}
	type result struct {
		ch  chan []byte
		err error
	}
	ready := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// CoInitializeEx(MTA); S_FALSE (уже инициализировано) — тоже ок.
		procCoInitializeEx.Call(0, coinitMultithreaded)
		defer procCoUninitialize.Call()

		lb, err := setupLoopback()
		if err != nil {
			ready <- result{nil, err}
			return
		}
		defer lb.close()

		stdin, out, err := startOpusFromPCM(ctx, lb.sampleFmt, lb.rate, lb.channels)
		if err != nil {
			ready <- result{nil, err}
			return
		}
		ready <- result{out, nil}

		lb.pump(ctx, stdin) // блокирует до ctx.Done / ошибки
		_ = stdin.Close()   // EOF → ffmpeg дококодирует и закроет stdout → out закроется
	}()
	r := <-ready
	if r.err != nil {
		return nil, fmt.Errorf("wasapi audio: %w", r.err)
	}
	return r.ch, nil
}
