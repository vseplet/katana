//go:build windows

package capture

// Захват экрана/окна через Windows.Graphics.Capture (WGC): создаём D3D11-устройство,
// GraphicsCaptureItem (для монитора или окна), free-threaded framepool и сессию,
// затем в отдельном (залоченном) потоке опрашиваем TryGetNextFrame, копируем BGRA-
// кадр на CPU, конвертируем в NV12 и кодируем H264 MFT'ом. Результат — access
// unit'ы в канал (тот же контракт, что x11grab/VideoToolbox: H264 Annex-B).
//
// Free-threaded framepool не требует DispatcherQueue и позволяет опрашивать кадры
// из своего потока без цикла сообщений — поэтому обходимся без делегата FrameArrived.

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

var procMonitorFromPoint = user32.NewProc("MonitorFromPoint")

const monitorDefaultToPrimary = 1

// IGraphicsCaptureSession2 — управление захватом курсора (put на индексе 7).
var iidIGraphicsCaptureSession2 = mustGUID("{2C39AE40-7D2E-5044-804E-8B6799D4CF9E}")

const (
	idxInteropCreateForWindow  = 3 // IGraphicsCaptureItemInterop::CreateForWindow
	idxInteropCreateForMonitor = 4 // CreateForMonitor
	idxFramePoolTryGetNext     = 7 // IDirect3D11CaptureFramePool::TryGetNextFrame
	idxFramePoolCreateSession  = 8 // CreateCaptureSession
	idxStatics2CreateFree      = 6 // IDirect3D11CaptureFramePoolStatics2::CreateFreeThreaded
	idxFrameGetSurface         = 6 // IDirect3D11CaptureFrame::get_Surface
	idxSessionStartCapture     = 6 // IGraphicsCaptureSession::StartCapture
	idxSession2PutCursor       = 7 // IGraphicsCaptureSession2::put_IsCursorCaptureEnabled

	wgcNumBuffers = 2
)

// videoProbe — доступен ли нативный видео-путь: поднимается ли D3D11-устройство и
// создаётся ли H264 MFT. Кэшируется; выполняется на залоченном потоке с COM.
var (
	probeOnce sync.Once
	probeOK   bool
)

func videoProbe() bool {
	probeOnce.Do(func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := roInitialize(); err != nil {
			log.Printf("capture: RoInitialize: %v", err)
			return
		}
		defer roUninitialize()

		dev, ctx, err := createD3D11Device()
		if err != nil {
			log.Printf("capture: no D3D11 device (video disabled): %v", err)
			return
		}
		comRelease(ctx)
		comRelease(dev)
		if err := probeH264Encoder(); err != nil {
			log.Printf("capture: %v (video disabled)", err)
			return
		}
		if debugCapture() {
			logEncoders() // диагностика: какие H264-энкодеры доступны (hardware/software)
		}
		probeOK = true
		log.Printf("capture: windows native video available (WGC + H264 MFT)")
	})
	return probeOK
}

// wgcSession держит энкодер и сессию, обеспечивая потокобезопасные хуки контроля
// (форс IDR / битрейт / курсор), которые дёргаются из WebRTC-контура.
type wgcSession struct {
	mu        sync.Mutex
	enc       *h264Encoder    // нативный MFT-fallback (старый syscall-путь)
	ff        *ffmpegVideoEnc // ffmpeg-путь (h264_amf/…, обычная сборка)
	nativeEnc winVideoEncoder // in-process аппаратный энкодер (сборка winnative)
	session2  uintptr

	// ctx/frames нужны firstFrameSetup, чтобы поднять читателя stdout ffmpeg.
	ctx    context.Context
	frames chan []byte
}

// winVideoEncoder — общий интерфейс для in-process нативного энкодера (реализация
// зависит от сборки: cgo+Media Foundation под тегом winnative, иначе — нет).
// submit кладёт NV12-кадр; готовые Annex-B AU энкодер сам гонит в frames.
type winVideoEncoder interface {
	submit(nv12 []byte) error
	forceKeyframe()
	setBitrate(kbps int)
	Close()
}

func (s *wgcSession) setEnc(e *h264Encoder) { s.mu.Lock(); s.enc = e; s.mu.Unlock() }

func (s *wgcSession) forceKeyframe() {
	s.mu.Lock()
	e := s.enc
	ne := s.nativeEnc
	s.mu.Unlock()
	if e != nil {
		e.forceKeyframe()
	}
	if ne != nil {
		ne.forceKeyframe()
	}
}

func (s *wgcSession) setBitrate(kbps int) {
	s.mu.Lock()
	e := s.enc
	ne := s.nativeEnc
	s.mu.Unlock()
	if e != nil {
		e.setBitrate(kbps)
	}
	if ne != nil {
		ne.setBitrate(kbps)
	}
}

func (s *wgcSession) setCursor(show bool) {
	s.mu.Lock()
	sess2 := s.session2
	s.mu.Unlock()
	if sess2 != 0 {
		var v uintptr
		if show {
			v = 1
		}
		comCall(sess2, idxSession2PutCursor, v)
	}
}

// startVideoWGC поднимает захват. Настройка идёт в самом потоке захвата (COM-объекты
// используем из создавшего их потока); результат настройки возвращаем через ready.
func startVideoWGC(ctx context.Context, opts Options) (chan []byte, *streamCtl, error) {
	frames := make(chan []byte, 4)
	sess := &wgcSession{}
	ready := make(chan error, 1)
	go sess.run(ctx, opts, frames, ready)
	if err := <-ready; err != nil {
		return nil, nil, err
	}
	ctl := &streamCtl{
		forceKeyframe: sess.forceKeyframe,
		setBitrate:    sess.setBitrate,
		setCursor:     sess.setCursor,
	}
	return frames, ctl, nil
}

// run — весь жизненный цикл захвата на одном залоченном потоке.
func (s *wgcSession) run(ctx context.Context, opts Options, frames chan []byte, ready chan error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(frames)
	s.ctx, s.frames = ctx, frames

	if err := roInitialize(); err != nil {
		ready <- err
		return
	}
	defer roUninitialize()

	// Инициализация WGC в VM (Parallels) бывает флейки: RoGetActivationFactory
	// изредка отдаёт E_INVALIDARG на ровном месте, хотя предыдущий запуск поднимался.
	// Повторяем настройку несколько раз с паузой — обычно следующая попытка проходит.
	// Частично созданные COM-объекты неудачной попытки освобождаем, чтобы не течь.
	var dev, devCtx, item, pool, statics2, session uintptr
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		dev, devCtx, item, pool, statics2, session, err = s.setup(opts)
		if err == nil {
			break
		}
		log.Printf("capture: wgc setup попытка %d/4: %v", attempt, err)
		s.releasePartial(dev, devCtx, item, pool, statics2, session)
		dev, devCtx, item, pool, statics2, session = 0, 0, 0, 0, 0, 0
		select {
		case <-ctx.Done():
			ready <- ctx.Err()
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err != nil {
		ready <- err
		return
	}
	defer func() {
		// ff.Close() дожидается выхода читателя stdout — обязан отработать до
		// close(frames) (тот defer зарегистрирован раньше → выполнится позже).
		if s.ff != nil {
			s.ff.Close()
		}
		if s.nativeEnc != nil {
			s.nativeEnc.Close()
		}
		if s.enc != nil {
			s.enc.Close()
		}
		winrtClose(session)
		comRelease(session)
		if s.session2 != 0 {
			comRelease(s.session2)
		}
		winrtClose(pool)
		comRelease(pool)
		comRelease(statics2)
		comRelease(item)
		comRelease(devCtx)
		comRelease(dev)
	}()
	ready <- nil

	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}
	kbps := parseBitrateKbps(opts.Bitrate)

	var staging uintptr
	var nv12 []byte
	var srcW, srcH, dstW, dstH int // src — размер кадра WGC; dst — размер энкода (даунскейл)
	defer func() {
		if staging != 0 {
			comRelease(staging)
		}
	}()

	frameDur := time.Second / time.Duration(fps)

	// Event-driven + CFR-заполнение простоя. Ключ к отсутствию дрожания на движении:
	// эмитить РЕАЛЬНЫЙ кадр WGC сразу, как он пришёл (1:1 с источником, как на
	// mac/Linux) — НЕ по своей 60fps-сетке. Отдельный тикер, не синхронный с подачей
	// WGC, давал биения (дубль на паузе / дроп при двух кадрах в интервале) = тряска.
	// Поэтому поллим быстрее fps (ловим кадр с малой задержкой), а простой (WGC даёт
	// ~10fps, когда экран статичен) добиваем повторами до fps — чтобы клиент видел
	// ровный CFR и RTP-часы шли в ногу с реальным временем.
	pollTicker := time.NewTicker(time.Second / time.Duration(fps*4))
	defer pollTicker.Stop()

	// Тайминги: раз в ~2с логируем fps, долю реальных/заполняющих кадров и convert/encode.
	var nFrames, nFill, nReal int
	var sumConv, sumEnc time.Duration
	statT := time.Now()

	var haveNV12 bool // получен хотя бы один кадр (есть что кодировать/повторять)
	var convDur time.Duration
	var nextFill time.Time // дедлайн следующего CFR-повтора при простое

	// emit кодирует текущий nv12 и шлёт access unit'ы. false → ctx отменён, выходим.
	emit := func() bool {
		te := time.Now()
		// Нативный in-process энкодер (winnative): submit NV12, забираем готовые AU.
		if s.nativeEnc != nil {
			// Энкодер сам гонит готовые AU в frames (poll-горутина → readH264KeepHeaders).
			if err := s.nativeEnc.submit(nv12); err != nil {
				log.Printf("capture: native encoder submit: %v", err)
				return false
			}
			sumEnc += time.Since(te)
			nFrames++
			return true
		}
		// Путь ffmpeg: кадр уходит в stdin, готовые AU шлёт читатель stdout.
		if s.ff != nil {
			if err := s.ff.writeFrame(nv12); err != nil {
				log.Printf("capture: ffmpeg video write: %v", err)
				return false // ffmpeg умер → останавливаем захват
			}
			sumEnc += time.Since(te)
			nFrames++
			return true
		}
		aus, encErr := s.enc.encode(nv12)
		sumEnc += time.Since(te)
		if encErr != nil {
			log.Printf("capture: encode: %v", encErr)
			return true
		}
		for _, au := range aus {
			if !pushFrame(ctx, frames, au, opts.DropLate) {
				return false
			}
		}
		nFrames++
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
		}

		// Забираем самый свежий кадр WGC (если есть) и конвертируем в nv12.
		convDur = 0
		gotNew := false
		if frame := latestFrame(pool); frame != 0 {
			_, surface := comCallOut(frame, idxFrameGetSurface)
			if tex, terr := surfaceToTexture(surface); terr == nil {
				// staging!=0 — единый признак готового энкодера (ffmpeg ИЛИ MFT):
				// оба пути ставят его только при полном успехе setup.
				if staging == 0 {
					s.firstFrameSetup(tex, dev, opts, fps, kbps, &staging, &nv12, &srcW, &srcH, &dstW, &dstH)
				}
				if staging != 0 {
					tc := time.Now()
					if data, rowPitch, unmap, merr := mapStaging(devCtx, staging, tex); merr == nil {
						// Кадр валиден только при ненулевых pitch/размерах и достаточном
						// приёмнике. Иначе (первый кадр / resize / флейк маппинга на amd64)
						// пропускаем — иначе bgraToNV12 паникует на пустом срезе.
						if data != 0 && rowPitch > 0 && srcW > 0 && srcH > 0 &&
							dstW > 0 && dstH > 0 && len(nv12) >= dstW*dstH*3/2 {
							bgraToNV12(data, rowPitch, srcW, srcH, dstW, dstH, nv12)
							haveNV12 = true
							gotNew = true
							convDur = time.Since(tc)
						} else if debugCapture() {
							log.Printf("wgc: skip invalid frame (pitch=%d src=%dx%d dst=%dx%d nv12=%d)",
								rowPitch, srcW, srcH, dstW, dstH, len(nv12))
						}
						unmap()
					}
				}
				comRelease(tex)
			}
			comRelease(surface)
			winrtClose(frame)
			comRelease(frame)
		}

		if !haveNV12 {
			continue // первого кадра ещё не было — кодировать нечего
		}

		now := time.Now()
		switch {
		case gotNew:
			// Реальный кадр — эмитим немедленно (без сетки → нет биений на движении).
			nReal++
			sumConv += convDur
			if !emit() {
				return
			}
			nextFill = now.Add(frameDur) // реальный кадр сдвигает дедлайн заполнения
		case now.After(nextFill):
			// Простой/сталл — держим fps повтором (RTP идёт в ногу с реальным временем).
			nFill++
			if !emit() {
				return
			}
			nextFill = nextFill.Add(frameDur)
			if now.Sub(nextFill) > frameDur {
				nextFill = now.Add(frameDur) // не копим долг после долгой паузы
			}
		}

		if el := time.Since(statT); el >= 2*time.Second {
			if debugCapture() {
				cv := 0.0
				if nReal > 0 {
					cv = float64(sumConv.Microseconds()) / float64(nReal) / 1000
				}
				log.Printf("capture/perf: %.1ffps (real=%d fill=%d) convert=%.1fms/кадр encode=%.1fms (n=%d/%.1fs)",
					float64(nFrames)/el.Seconds(), nReal, nFill, cv,
					float64(sumEnc.Microseconds())/float64(nFrames+1)/1000,
					nFrames, el.Seconds())
			}
			nFrames, nFill, nReal, sumConv, sumEnc, statT = 0, 0, 0, 0, 0, time.Now()
		}
	}
}

// firstFrameSetup поднимает staging-текстуру и H264-энкодер по размеру первого
// кадра (даунскейл до opts.Width). Ошибки логирует и оставляет s.enc == nil —
// цикл попробует снова на следующем кадре.
func (s *wgcSession) firstFrameSetup(tex, dev uintptr, opts Options, fps, kbps int,
	staging *uintptr, nv12 *[]byte, srcW, srcH, dstW, dstH *int) {
	d := textureDesc(tex)
	*srcW, *srcH = int(d.Width)&^1, int(d.Height)&^1 // NV12 требует чётные размеры
	if *srcW <= 0 || *srcH <= 0 {
		return
	}
	// Даунскейл до запрошенной ширины: софт-H264 и Go-конвертация линейны по
	// пикселям, 1920→1280 срезает нагрузку, но fps держим целевой (CFR-тикер).
	*dstW, *dstH = scaleDims(*srcW, *srcH, opts.Width)
	st, serr := createStagingTexture(dev, uint32(*srcW), uint32(*srcH))
	if serr != nil {
		log.Printf("capture: staging texture: %v", serr)
		return
	}

	// In-process нативный энкодер (сборка winnative): аппаратный H264 MFT на ТОМ ЖЕ
	// D3D11-устройстве, что и захват — общий GPU-контекст, без ffmpeg-подпроцесса и
	// второго девайса. В обычной сборке newNativePreferredEncoder возвращает ok=false.
	if ne, ok := newNativePreferredEncoder(s.ctx, s.frames, dev, *dstW, *dstH, fps, kbps, fps*2, opts.DropLate); ok {
		s.mu.Lock()
		s.nativeEnc = ne
		s.mu.Unlock()
		*staging = st
		*nv12 = make([]byte, (*dstW)*(*dstH)*3/2)
		log.Printf("capture: wgc src %dx%d → enc %dx%d @ %dfps %dkbps (native MF, shared D3D)",
			*srcW, *srcH, *dstW, *dstH, fps, kbps)
		return
	}
	if preferNativeOnly {
		// Нативная сборка: ffmpeg/MFT-фолбэка нет — чинить надо натив (HRESULT выше).
		log.Printf("capture: native-only build — энкодер не поднялся, видео нет")
		comRelease(st)
		return
	}

	// Предпочитаем ffmpeg (аппаратный h264_amf/nvenc/qsv, вендор-нейтрально).
	// KATANA_MFT_ENCODER форсит нативный MFT (диагностика/машины без ffmpeg).
	if os.Getenv("KATANA_MFT_ENCODER") == "" {
		if ff, ferr := newFFmpegVideoEnc(s.ctx, s.frames, *dstW, *dstH, fps, kbps, opts.DropLate); ferr == nil {
			s.mu.Lock()
			s.ff = ff
			s.mu.Unlock()
			*staging = st
			*nv12 = make([]byte, (*dstW)*(*dstH)*3/2)
			log.Printf("capture: wgc src %dx%d → enc %dx%d @ %dfps %dkbps (ffmpeg %s, CFR)",
				*srcW, *srcH, *dstW, *dstH, fps, kbps, ff.name)
			return
		} else {
			log.Printf("capture: ffmpeg video unavailable (%v) — fallback native MFT", ferr)
		}
	}

	enc, eerr := newH264Encoder(dev, *dstW, *dstH, fps, kbps)
	if eerr != nil {
		log.Printf("capture: h264 encoder: %v", eerr)
		comRelease(st)
		return
	}
	*staging = st
	*nv12 = make([]byte, (*dstW)*(*dstH)*3/2)
	s.setEnc(enc)
	log.Printf("capture: wgc src %dx%d → enc %dx%d @ %dfps %dkbps (H264 MFT, CFR)",
		*srcW, *srcH, *dstW, *dstH, fps, kbps)
}

// setup создаёт D3D-устройство, capture item, framepool и сессию, стартует захват.
func (s *wgcSession) setup(opts Options) (dev, devCtx, item, pool, statics2, session uintptr, err error) {
	// diag — подробные логи настройки только под KATANA_DEBUG (в обычном режиме тихо).
	diag := func(format string, args ...any) {
		if debugCapture() {
			log.Printf(format, args...)
		}
	}
	dev, devCtx, err = createD3D11Device()
	if err != nil {
		return
	}
	winrtDev, derr := d3dDeviceToWinRT(dev)
	if derr != nil {
		err = derr
		return
	}
	defer comRelease(winrtDev)

	diag("wgc/diag: dev=%#x devCtx=%#x winrtDev=%#x", dev, devCtx, winrtDev)

	item, err = s.captureItem(opts)
	if err != nil {
		return
	}
	diag("wgc/diag: item=%#x", item)
	// Пробинг item: get_Size (индекс 7). Валидный item вернёт hr=0 и разумные WxH.
	{
		sz := new(struct{ w, h int32 })
		hrSz := comCall(item, 7, uintptr(unsafe.Pointer(sz)))
		runtime.KeepAlive(sz)
		diag("wgc/diag: item.Size hr=%#x %dx%d", uint32(hrSz), sz.w, sz.h)
	}

	// Размер framepool — из прямоугольника источника (монитор или окно).
	rect, _ := SourceRect(opts.SourceKind, opts.SourceID)
	pw, ph := int32(rect.W), int32(rect.H)
	if pw <= 0 || ph <= 0 {
		pw, ph = 1920, 1080
	}

	statics2, err = activationFactory(classDirect3D11CaptureFramePool, &iidIDirect3D11CaptureFramePoolStat2)
	if err != nil {
		return
	}
	diag("wgc/diag: statics2=%#x size=%dx%d", statics2, pw, ph)
	size := uintptr(uint32(pw)) | uintptr(uint32(ph))<<32 // SizeInt32 by value
	hr, poolOut := comCallOut(statics2, idxStatics2CreateFree,
		winrtDev, uintptr(dxgiFormatB8G8R8A8Unorm), uintptr(wgcNumBuffers), size)
	pool = poolOut
	diag("wgc/diag: CreateFreeThreaded hr=%#x pool=%#x", uint32(hr), pool)
	if err = hrError(hr, "CreateFreeThreaded"); err != nil {
		return
	}
	// Пробинг pool: TryGetNextFrame (индекс 7). До StartCapture обычно hr=0, frame=0.
	// Если vtable pool «съехала» — увидим мусор/ошибку тут.
	if ptrReadable(pool) {
		hrTf, f := comCallOut(pool, idxFramePoolTryGetNext)
		diag("wgc/diag: pool.TryGetNextFrame hr=%#x frame=%#x", uint32(hrTf), f)
		if f != 0 {
			winrtClose(f)
			comRelease(f)
		}
	}

	if !ptrReadable(pool) {
		err = fmt.Errorf("CreateFreeThreaded вернул нечитаемый pool=%#x", pool)
		return
	}
	// Авто-определение индекса CreateCaptureSession в vtable framepool. В IDL событие
	// FrameArrived может стоять ПЕРЕД CreateCaptureSession, тогда индекс не 8, а 10.
	// Перебираем кандидатов и берём тот, что вернул hr=0 и ЧИТАЕМЫЙ указатель
	// (add_FrameArrived вернёт EventRegistrationToken — как указатель нечитаемый —
	// и отсеется; getDispatcherQueue(11) не трогаем, у него другая раскладка arg'ов).
	for _, idx := range []int{10, 8, 9} {
		hrC, cand := comCallOut(pool, idx, item)
		diag("wgc/diag: CreateCaptureSession probe idx=%d hr=%#x ptr=%#x readable=%v",
			idx, uint32(hrC), cand, ptrReadable(cand))
		if hrC == sOK && ptrReadable(cand) {
			session = cand
			diag("wgc/diag: CreateCaptureSession найден на индексе %d", idx)
			break
		}
	}
	if session == 0 {
		err = fmt.Errorf("CreateCaptureSession: не найден рабочий индекс vtable")
		return
	}

	// Захват курсора — best-effort (IGraphicsCaptureSession2). Опционален.
	if sess2, qerr := comQueryInterface(session, &iidIGraphicsCaptureSession2); qerr == nil {
		s.session2 = sess2
		var cv uintptr
		if opts.Cursor {
			cv = 1
		}
		comCall(sess2, idxSession2PutCursor, cv)
	}

	if hr := comCall(session, idxSessionStartCapture); hr != sOK {
		err = hrError(hr, "StartCapture")
		return
	}
	return
}

// releasePartial освобождает COM-объекты частично удавшейся setup (для ретрая).
// Порядок обратный созданию; s.session2 (курсорный QI) тоже сбрасываем.
func (s *wgcSession) releasePartial(dev, devCtx, item, pool, statics2, session uintptr) {
	if session != 0 {
		winrtClose(session)
		comRelease(session)
	}
	if s.session2 != 0 {
		comRelease(s.session2)
		s.session2 = 0
	}
	if pool != 0 {
		winrtClose(pool)
		comRelease(pool)
	}
	if statics2 != 0 {
		comRelease(statics2)
	}
	if item != 0 {
		comRelease(item)
	}
	if devCtx != 0 {
		comRelease(devCtx)
	}
	if dev != 0 {
		comRelease(dev)
	}
}

// captureItem создаёт GraphicsCaptureItem для окна (SourceKind=="window"/"app",
// SourceID=HWND) или для первичного монитора.
func (s *wgcSession) captureItem(opts Options) (uintptr, error) {
	interop, err := activationFactory(classGraphicsCaptureItem, &iidIGraphicsCaptureItemInterop)
	if err != nil {
		return 0, err
	}
	defer comRelease(interop)

	if (opts.SourceKind == "window" || opts.SourceKind == "app") && opts.SourceID != 0 {
		hr, item := comCallOut(interop, idxInteropCreateForWindow,
			uintptr(opts.SourceID), uintptr(unsafe.Pointer(&iidIGraphicsCaptureItem)))
		if err := hrError(hr, "CreateForWindow"); err != nil {
			return 0, err
		}
		return item, nil
	}
	hmon, _, _ := procMonitorFromPoint.Call(0, monitorDefaultToPrimary) // первичный монитор
	hr, item := comCallOut(interop, idxInteropCreateForMonitor,
		hmon, uintptr(unsafe.Pointer(&iidIGraphicsCaptureItem)))
	if err := hrError(hr, "CreateForMonitor"); err != nil {
		return 0, err
	}
	return item, nil
}

// latestFrame опустошает framepool до самого свежего кадра (сброс отставания —
// меньше задержка), возвращая последний. 0, если новых кадров нет.
func latestFrame(pool uintptr) uintptr {
	var last uintptr
	for {
		_, f := comCallOut(pool, idxFramePoolTryGetNext)
		if f == 0 {
			break
		}
		if last != 0 {
			winrtClose(last)
			comRelease(last)
		}
		last = f
	}
	return last
}

// scaleDims — целевой размер энкода: если задана opts.Width меньше исходной ширины,
// масштабируем с сохранением пропорций (чётные размеры для NV12), иначе — как есть.
func scaleDims(srcW, srcH, targetW int) (int, int) {
	if targetW <= 0 || targetW >= srcW {
		return srcW &^ 1, srcH &^ 1
	}
	dw := targetW &^ 1
	dh := (srcH * dw / srcW) &^ 1
	if dh <= 0 {
		dh = 2
	}
	return dw, dh
}

// bgraToNV12 конвертирует BGRA (mapped, с учётом rowPitch) в NV12 (BT.601, studio
// range) с одновременным даунскейлом src→dst (nearest-neighbor). Хрома — 2×2
// субдискретизацией. Таблица столбцов sxTab предвычислена (без деления в hot-loop).
//
// Распараллелено по строкам: на full-res десктопе однопоточная конвертация ~15–20мс
// на кадр упирала захват в ~50fps (энкод уже на GPU, а это оставалось на одном ядре).
// Делим строки на диапазоны по числу ядер; каждый воркер считает свой кусок Y+UV в
// НЕПЕРЕСЕКАЮЩИЕСЯ области dst (границы чётные, чтобы UV-строки не делились).
func bgraToNV12(src uintptr, rowPitch, srcW, srcH, dstW, dstH int, dst []byte) {
	// Защита от невалидного кадра (нулевые pitch/размеры или маленький приёмник):
	// молча пропускаем, иначе обращение к px[0] паникует на пустом срезе.
	if src == 0 || rowPitch <= 0 || srcW <= 0 || srcH <= 0 || dstW <= 0 || dstH <= 0 || len(dst) < dstW*dstH*3/2 {
		return
	}
	px := unsafe.Slice((*byte)(unsafe.Pointer(src)), rowPitch*srcH)
	sxTab := make([]int, dstW)
	for x := 0; x < dstW; x++ {
		sxTab[x] = (x * srcW / dstW) * 4 // байтовый сдвиг исходного пикселя (BGRA=4Б)
	}

	nw := convWorkers(dstH)
	if nw <= 1 {
		convRows(px, rowPitch, srcH, dstW, dstH, sxTab, dst, 0, dstH)
		return
	}
	chunk := (dstH + nw - 1) / nw
	if chunk%2 == 1 {
		chunk++ // чётная граница: UV-строка (каждая 2-я) целиком в одном воркере
	}
	var wg sync.WaitGroup
	for y0 := 0; y0 < dstH; y0 += chunk {
		y1 := y0 + chunk
		if y1 > dstH {
			y1 = dstH
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			convRows(px, rowPitch, srcH, dstW, dstH, sxTab, dst, y0, y1)
		}(y0, y1)
	}
	wg.Wait()
}

// convRows конвертирует диапазон строк [y0,y1) (Y-плоскость + UV для чётных строк).
// UV-смещение считается из индекса строки (uvBase + (y/2)*dstW + x), а не бегущим
// счётчиком — чтобы диапазоны были независимы и параллелились без гонок.
func convRows(px []byte, rowPitch, srcH, dstW, dstH int, sxTab []int, dst []byte, y0, y1 int) {
	uvBase := dstW * dstH
	for y := y0; y < y1; y++ {
		row := (y * srcH / dstH) * rowPitch
		yo := y * dstW
		for x := 0; x < dstW; x++ {
			i := row + sxTab[x]
			b, g, r := int(px[i]), int(px[i+1]), int(px[i+2])
			dst[yo+x] = clamp8(((66*r + 129*g + 25*b + 128) >> 8) + 16)
		}
		if y&1 == 0 { // UV — по чётным строкам (2×2 субдискретизация)
			uvRow := uvBase + (y/2)*dstW
			for x := 0; x < dstW; x += 2 {
				i := row + sxTab[x]
				b, g, r := int(px[i]), int(px[i+1]), int(px[i+2])
				dst[uvRow+x] = clamp8(((-38*r - 74*g + 112*b + 128) >> 8) + 128)
				dst[uvRow+x+1] = clamp8(((112*r - 94*g - 18*b + 128) >> 8) + 128)
			}
		}
	}
}

// convWorkers — сколько горутин пускать на конвертацию кадра: по числу ядер, но с
// потолком (убывающая отдача + оставляем ядра под захват/pump/энкод), и не дробим
// мелкие кадры (накладные расходы на горутины > выигрыш).
func convWorkers(dstH int) int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 || dstH < 2*n {
		return 1
	}
	return n
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// parseBitrateKbps парсит opts.Bitrate ("3M"/"3000k"/"3000") в kbps (дефолт 3000).
// Аналог bitrateKbps на Linux (тот под тегом linux, поэтому здесь свой).
func parseBitrateKbps(b string) int {
	b = strings.TrimSpace(strings.ToLower(b))
	mult := 1
	switch {
	case strings.HasSuffix(b, "m"):
		mult, b = 1000, strings.TrimSuffix(b, "m")
	case strings.HasSuffix(b, "k"):
		mult, b = 1, strings.TrimSuffix(b, "k")
	}
	n, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil || n <= 0 {
		return 3000
	}
	return n * mult
}

// streamCtl — управляющие хуки нативного энкодера для WebRTC-контура.
type streamCtl struct {
	forceKeyframe func()
	setBitrate    func(kbps int)
	setCursor     func(show bool)
}
