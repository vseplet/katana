//go:build windows

package capture

// Аппаратный H264-энкодер на Media Foundation. В отличие от софтового
// CMSH264EncoderMFT (синхронный, кодирует на CPU и на физической машине в полном
// разрешении не влезал в бюджет кадра → overload на движении), аппаратные MFT
// (NVIDIA NVENC / Intel QuickSync / AMD AMF) кодируют на GPU почти бесплатно для
// CPU. Такие MFT АСИНХРОННЫЕ: после MFT_MESSAGE_NOTIFY_START_OF_STREAM они шлют
// события METransformNeedInput / METransformHaveOutput через IMFMediaEventGenerator,
// и заблокированы до установки MF_TRANSFORM_ASYNC_UNLOCK.
//
// Настройка типов/битрейта/VBV — общая с софтовым путём (h264Encoder.configure);
// различается только прокачка кадров: encodeAsync ниже. Вход — тот же NV12 в
// системной памяти (CPU-конвертация из BGRA остаётся), кодирование уходит на GPU.

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Индексы vtable для аппаратного пути.
const (
	idxTransformGetAttributes = 8  // IMFTransform::GetAttributes
	idxActivateObject         = 33 // IMFActivate::ActivateObject
	idxActivateShutdownObject = 34 // IMFActivate::ShutdownObject
	idxEventGenGetEvent       = 3  // IMFMediaEventGenerator::GetEvent
	idxEventGenQueueEvent     = 6  // IMFMediaEventGenerator::QueueEvent
	idxEventGetType           = 33 // IMFMediaEvent::GetType (IMFAttributes+GetType)
)

const (
	mfEnumFlagHardware = 0x4        // MFT_ENUM_FLAG_HARDWARE
	mfEventFlagNoWait  = 0x1        // MF_EVENT_FLAG_NO_WAIT
	mfeNoEvents        = 0xC00D3E80 // MF_E_NO_EVENTS

	meTransformNeedInput  = 601 // METransformNeedInput
	meTransformHaveOutput = 602 // METransformHaveOutput
	meError               = 1   // MEError — фиктивное событие для пробуждения GetEvent
)

var (
	iidIMFMediaEventGenerator = mustGUID("{2CD0BD52-BCD5-4B89-B62C-EADC0C031E7D}")
	mfTransformAsyncUnlock    = mustGUID("{E5666D6B-3422-4EB6-A421-DA7DB1F8E207}") // MF_TRANSFORM_ASYNC_UNLOCK
	guidNull                  windows.GUID                                        // GUID_NULL для QueueEvent
)

// newHardwareH264Encoder поднимает первый доступный аппаратный H264 MFT. MFStartup
// уже вызван в newH264Encoder; при неудаче освобождаем COM-объекты БЕЗ MFShutdown
// (баланс держит вызывающий), чтобы софтовый фолбэк не остался без Media Foundation.
func newHardwareH264Encoder(w, h, fps, kbps int) (*h264Encoder, error) {
	act, name, err := enumHardwareH264()
	if err != nil {
		return nil, err
	}
	hr, mft := comCallOut(act, idxActivateObject, uintptr(unsafe.Pointer(&iidIMFTransform)))
	runtime.KeepAlive(&iidIMFTransform)
	if e := hrError(hr, "ActivateObject"); e != nil {
		comRelease(act)
		return nil, e
	}
	e := &h264Encoder{mft: mft, activate: act, async: true, w: w, h: h, fps: fps}
	// Освобождение частично поднятого энкодера БЕЗ MFShutdown (в отличие от Close):
	// баланс MFStartup держит вызывающий (newH264Encoder), чтобы софтовый фолбэк не
	// остался без Media Foundation. Останавливаем pump, если уже запущен.
	fail := func(err error) (*h264Encoder, error) {
		e.stopPump() // no-op, если pump ещё не запущен
		if e.evGen != 0 {
			comRelease(e.evGen)
		}
		if e.codec != 0 {
			comRelease(e.codec)
		}
		comRelease(e.mft)
		comCall(e.activate, idxActivateShutdownObject)
		comRelease(e.activate)
		return nil, err
	}

	// Разблокировать асинхронный MFT (иначе последующие вызовы = MF_E_TRANSFORM_ASYNC_LOCKED).
	if err := e.asyncUnlock(); err != nil {
		return fail(err)
	}
	if err := e.configure(kbps); err != nil {
		return fail(err)
	}
	// Генератор событий (NeedInput/HaveOutput). Если его нет — это не async-MFT,
	// наш прокачивающий цикл не применим: считаем аппаратный путь непригодным.
	evGen, err := comQueryInterface(mft, &iidIMFMediaEventGenerator)
	if err != nil {
		return fail(fmt.Errorf("не async MFT (нет IMFMediaEventGenerator): %w", err))
	}
	e.evGen = evGen
	e.startPump()
	// Smoke-тест: HW-MFT может принять типы, но не отдавать выход с СИСТЕМНОЙ памятью
	// (AMF/NVENC порой требуют D3D-текстурный вход → ProcessInput = MF_E_NOTACCEPTING
	// или молчаливо нет выхода → чёрный экран). Гоним несколько кадров и ждём первый
	// AU; нет за таймаут — считаем HW непригодным, откат на софтовый (newH264Encoder).
	if !e.smokeTest() {
		return fail(fmt.Errorf("HW H264 (%s) не отдаёт выход с системной памятью (нужен D3D-вход) — откат на софт", name))
	}
	e.mu.Lock()
	e.forceKey = true // smoke-выход отброшен → первый реальный кадр делаем IDR
	e.mu.Unlock()
	log.Printf("mf: аппаратный H264 MFT (%s) %dx%d @ %dfps %dkbps", name, w, h, fps, kbps)
	return e, nil
}

// smokeTest прогоняет несколько чёрных кадров через async-конвейер и ждёт первый
// выходной AU. true — HW реально отдаёт H264 (пригоден); false — за таймаут ничего
// не вышло (принял вход, но молчит / ProcessInput=NOTACCEPTING — типично для MFT,
// требующих D3D-вход). Крутится в потоке настройки (WGC), до первого реального кадра.
func (e *h264Encoder) smokeTest() bool {
	ysz := e.w * e.h
	black := make([]byte, ysz*3/2)
	for i := 0; i < ysz; i++ {
		black[i] = 16 // Y = 16 (studio-range чёрный)
	}
	for i := ysz; i < len(black); i++ {
		black[i] = 128 // UV = 128 (нейтральная хрома)
	}
	ok := false
	for i := 0; i < 40; i++ { // ~40 кадров × 15мс ≈ 0.6с — с запасом на глубину конвейера
		if aus, err := e.encodeAsync(black); err == nil && len(aus) > 0 {
			ok = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	// Диагностика: по счётчикам видно, ПОЧЕМУ HW молчит (см. dbgNeedIn/dbgHaveOut).
	log.Printf("mf: smoke HW — ok=%v NeedInput=%d HaveOutput=%d",
		ok, atomic.LoadInt64(&e.dbgNeedIn), atomic.LoadInt64(&e.dbgHaveOut))
	return ok
}

// enumHardwareH264 перечисляет аппаратные H264-энкодеры MFT и возвращает первый
// (IMFActivate) + его дружественное имя. Остальные освобождает.
func enumHardwareH264() (act uintptr, name string, err error) {
	out := &mftRegisterTypeInfo{major: mfMediaTypeVideo, sub: mfVideoFormatH264}
	ppBase := new(uintptr)
	pCount := new(uint32)
	// GUID категории — по значению; ABI-раскладка в callMFTEnumEx (x64: указатель,
	// arm64: два регистра). input=NULL: не фильтруем по входному формату (часть
	// HW-энкодеров не регистрируют NV12 статически) — валидируем через SetInputType.
	hr := callMFTEnumEx(&catVideoEncoder, mfEnumFlagHardware, 0,
		uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(ppBase)), uintptr(unsafe.Pointer(pCount)))
	runtime.KeepAlive(out)
	runtime.KeepAlive(ppBase)
	runtime.KeepAlive(pCount)
	if hr != sOK {
		return 0, "", hrError(hr, "MFTEnumEx(hardware)")
	}
	count := int(*pCount)
	base := *ppBase
	if base == 0 || count == 0 {
		return 0, "", fmt.Errorf("аппаратных H264-энкодеров не найдено")
	}
	arr := unsafe.Slice((*uintptr)(unsafe.Pointer(base)), count)
	act = arr[0]
	name = activateFriendlyName(act)
	for i := 1; i < count; i++ {
		comRelease(arr[i])
	}
	procCoTaskMemFree.Call(base)
	return act, name, nil
}

// asyncUnlock ставит MF_TRANSFORM_ASYNC_UNLOCK=1 на атрибутах MFT — обязательно
// для аппаратных async-энкодеров, иначе SetInputType/ProcessInput = ASYNC_LOCKED.
func (e *h264Encoder) asyncUnlock() error {
	hr, attrs := comCallOut(e.mft, idxTransformGetAttributes)
	if err := hrError(hr, "GetAttributes"); err != nil {
		return err
	}
	if attrs == 0 {
		return fmt.Errorf("GetAttributes вернул nil")
	}
	defer comRelease(attrs)
	if hr := comCall(attrs, idxMTSetUINT32,
		uintptr(unsafe.Pointer(&mfTransformAsyncUnlock)), 1); hr != sOK {
		return hrError(hr, "SetUINT32(ASYNC_UNLOCK)")
	}
	runtime.KeepAlive(&mfTransformAsyncUnlock)
	return nil
}

// startPump поднимает прокачивающую горутину и её каналы. Захват общается с
// энкодером ТОЛЬКО через inCh/outCh и никогда не заходит в событийный цикл MFT.
func (e *h264Encoder) startPump() {
	e.inCh = make(chan []byte, 4)
	e.outCh = make(chan []byte, 8)
	e.pumpStop = make(chan struct{})
	e.pumpGone = make(chan struct{})
	go e.pump()
}

// pump — событийный цикл асинхронного MFT на своём потоке (MTA). Ждёт события
// БЛОКИРУЮЩЕ (GetEvent с ожиданием): async-MFT отдаёт события только ожидающему
// GetEvent, а NO_WAIT-опрос их не вытягивает (проверено: NeedInput=0). На NeedInput
// кормит свежайшим кадром из inCh (повтор last при простое — держим CFR), на
// HaveOutput тянет AU в outCh. Захват не стопорим — он в другой горутине. Выходит по
// pumpStop; блокирующий GetEvent будят фиктивным QueueEvent из stopPump.
func (e *h264Encoder) pump() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = roInitialize() // присоединяемся к MTA (COM-вызовы MFT с этого потока)
	defer roUninitialize()
	defer close(e.pumpGone)

	var last []byte // последний поданный кадр — для повтора, если NeedInput без свежего
	for {
		mt, ok, err := e.nextEvent(true) // блокирует до события (или буд-события из stopPump)
		select {
		case <-e.pumpStop:
			return
		default:
		}
		if err != nil {
			log.Printf("mf: async pump event: %v", err)
			return
		}
		if !ok {
			continue
		}
		switch mt {
		case meTransformNeedInput:
			atomic.AddInt64(&e.dbgNeedIn, 1)
			if frame := e.latestFrame(last); frame != nil {
				last = frame
				e.feed(frame)
			}
		case meTransformHaveOutput:
			atomic.AddInt64(&e.dbgHaveOut, 1)
			aus, derr := e.drainOneOutput()
			if derr != nil {
				log.Printf("mf: async pump output: %v", derr)
				return
			}
			for _, au := range aus {
				e.pushOut(au)
			}
		}
	}
}

// latestFrame осушает inCh до самого свежего кадра (сброс отставания — меньше
// задержка); если новых кадров нет, возвращает last (повтор под NeedInput → CFR).
func (e *h264Encoder) latestFrame(last []byte) []byte {
	f := last
	for {
		select {
		case nf := <-e.inCh:
			f = nf
		default:
			return f
		}
	}
}

// stopPump останавливает прокачивающую горутину: закрывает pumpStop и будит
// блокирующий GetEvent фиктивным событием (QueueEvent), затем ждёт выхода pump.
func (e *h264Encoder) stopPump() {
	if e.pumpStop == nil {
		return
	}
	close(e.pumpStop)
	// Разбудить блокирующий GetEvent — кладём фиктивное событие в очередь генератора.
	comCall(e.evGen, idxEventGenQueueEvent, uintptr(meError),
		uintptr(unsafe.Pointer(&guidNull)), 0, 0)
	runtime.KeepAlive(&guidNull)
	<-e.pumpGone
	e.pumpStop = nil
}

// feed подаёт один NV12-кадр в MFT (ProcessInput). true — успех.
func (e *h264Encoder) feed(nv12 []byte) bool {
	sample, err := e.makeInputSample(nv12)
	if err != nil {
		log.Printf("mf: makeInputSample(async): %v", err)
		return false
	}
	hr := comCall(e.mft, idxMFTProcessInput, 0, sample, 0)
	comRelease(sample)
	if hr != sOK {
		log.Printf("mf: ProcessInput(async): 0x%08x", uint32(hr))
		return false
	}
	return true
}

// pushOut кладёт готовый AU в outCh; при отставании потребителя выкидывает старейший
// (свежесть кадра важнее — то же правило, что у DropLate в захвате).
func (e *h264Encoder) pushOut(au []byte) {
	select {
	case e.outCh <- au:
	default:
		select {
		case <-e.outCh:
		default:
		}
		select {
		case e.outCh <- au:
		default:
		}
	}
}

// encodeAsync (под e.async) — сторона захвата: кладёт КОПИЮ кадра в inCh (буфер
// захвата переиспользуется, а pump прочитает позже) и без ожидания забирает готовые
// access unit'ы. Выход отстаёт от входа на глубину конвейера (пара кадров) — для
// low-latency без B-кадров приемлемо.
func (e *h264Encoder) encodeAsync(nv12 []byte) ([][]byte, error) {
	buf := make([]byte, len(nv12))
	copy(buf, nv12)
	select {
	case e.inCh <- buf:
	default:
		// pump не успевает — выкидываем старейший кадр, кладём свежий (drop-late).
		select {
		case <-e.inCh:
		default:
		}
		select {
		case e.inCh <- buf:
		default:
		}
	}
	var out [][]byte
	for {
		select {
		case au := <-e.outCh:
			out = append(out, au)
		default:
			return out, nil
		}
	}
}

// nextEvent тянет одно событие из IMFMediaEventGenerator. block=false → без
// ожидания (MF_E_NO_EVENTS → ok=false). Возвращает тип события.
func (e *h264Encoder) nextEvent(block bool) (mt uint32, ok bool, err error) {
	flags := uintptr(mfEventFlagNoWait)
	if block {
		flags = 0
	}
	hr, ev := comCallOut(e.evGen, idxEventGenGetEvent, flags)
	if uint32(hr) == mfeNoEvents {
		return 0, false, nil
	}
	if hr != sOK || ev == 0 {
		return 0, false, hrError(hr, "GetEvent")
	}
	pType := new(uint32)
	comCall(ev, idxEventGetType, uintptr(unsafe.Pointer(pType)))
	runtime.KeepAlive(pType)
	mt = *pType
	comRelease(ev)
	return mt, true, nil
}

// drainOneOutput вытягивает один готовый выходной сэмпл (по METransformHaveOutput).
// Аппаратные MFT обычно сами аллоцируют сэмплы (selfAlloc); логика приведения к
// Annex-B — общая с синхронным путём.
func (e *h264Encoder) drainOneOutput() ([][]byte, error) {
	var outSample uintptr
	if !e.selfAlloc {
		s, err := e.makeOutputSample()
		if err != nil {
			return nil, err
		}
		outSample = s
	}
	odb := new(mftOutputDataBuffer)
	odb.pSample = outSample
	status := new(uint32)
	hr := comCall(e.mft, idxMFTProcessOutput, 0, 1,
		uintptr(unsafe.Pointer(odb)), uintptr(unsafe.Pointer(status)))
	runtime.KeepAlive(odb)
	runtime.KeepAlive(status)
	if uint32(hr) == mfeTransformNeedMoreInput || uint32(hr) == mfeTransformStreamChange {
		if outSample != 0 {
			comRelease(outSample)
		}
		return nil, nil
	}
	if hr != sOK {
		if outSample != 0 {
			comRelease(outSample)
		}
		return nil, hrError(hr, "ProcessOutput(async)")
	}
	produced := odb.pSample
	var out [][]byte
	if au := e.readSample(produced); au != nil {
		out = append(out, au)
		if debugCapture() && auHasKeyframe(au) {
			log.Printf("mf/codec: keyframe @ кадр %d (async, %d Б)", e.frame, len(au))
		}
	}
	comRelease(produced)
	return out, nil
}
