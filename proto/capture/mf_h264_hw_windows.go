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
	"unsafe"
)

// Индексы vtable для аппаратного пути.
const (
	idxTransformGetAttributes = 8  // IMFTransform::GetAttributes
	idxActivateObject         = 33 // IMFActivate::ActivateObject
	idxActivateShutdownObject = 34 // IMFActivate::ShutdownObject
	idxEventGenGetEvent       = 3  // IMFMediaEventGenerator::GetEvent
	idxEventGetType           = 33 // IMFMediaEvent::GetType (IMFAttributes+GetType)
)

const (
	mfEnumFlagHardware = 0x4        // MFT_ENUM_FLAG_HARDWARE
	mfEventFlagNoWait  = 0x1        // MF_EVENT_FLAG_NO_WAIT
	mfeNoEvents        = 0xC00D3E80 // MF_E_NO_EVENTS

	meTransformNeedInput  = 601 // METransformNeedInput
	meTransformHaveOutput = 602 // METransformHaveOutput
)

var (
	iidIMFMediaEventGenerator = mustGUID("{2CD0BD52-BCD5-4B89-B62C-EADC0C031E7D}")
	mfTransformAsyncUnlock    = mustGUID("{E5666D6B-3422-4EB6-A421-DA7DB1F8E207}") // MF_TRANSFORM_ASYNC_UNLOCK
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
	// Освобождение частично поднятого энкодера БЕЗ MFShutdown (в отличие от Close).
	fail := func(err error) (*h264Encoder, error) {
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
	log.Printf("mf: аппаратный H264 MFT (%s) %dx%d @ %dfps %dkbps", name, w, h, fps, kbps)
	return e, nil
}

// enumHardwareH264 перечисляет аппаратные H264-энкодеры MFT и возвращает первый
// (IMFActivate) + его дружественное имя. Остальные освобождает.
func enumHardwareH264() (act uintptr, name string, err error) {
	cat := catVideoEncoder
	pcat := unsafe.Pointer(&cat)
	lo := *(*uint64)(pcat)                              // GUID категории по значению
	hi := *(*uint64)(unsafe.Pointer(uintptr(pcat) + 8)) // (16 байт → два uint64)
	out := &mftRegisterTypeInfo{major: mfMediaTypeVideo, sub: mfVideoFormatH264}
	ppBase := new(uintptr)
	pCount := new(uint32)
	// input=NULL: не фильтруем по входному формату (часть HW-энкодеров не
	// регистрируют NV12 статически) — валидируем позже через SetInputType.
	hr, _, _ := procMFTEnumEx.Call(uintptr(lo), uintptr(hi), mfEnumFlagHardware, 0,
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

// encodeAsync прокачивает один NV12-кадр через асинхронный MFT: ждёт запрос ввода
// (собирая по пути готовый выход), подаёт кадр, затем без ожидания забирает готовые
// access unit'ы. Выход отстаёт от входа на глубину конвейера энкодера (пара кадров)
// — для low-latency без B-кадров это приемлемо. Вызывается из encode() под e.async.
func (e *h264Encoder) encodeAsync(nv12 []byte) ([][]byte, error) {
	var out [][]byte
	// 1. Дожидаемся METransformNeedInput (по пути обрабатывая готовый выход).
	for e.pendingIn == 0 {
		mt, ok, err := e.nextEvent(true)
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}
		if aus, err := e.onEvent(mt); err != nil {
			return out, err
		} else {
			out = append(out, aus...)
		}
	}
	// 2. Подаём кадр.
	if e.pendingIn > 0 {
		sample, err := e.makeInputSample(nv12)
		if err != nil {
			return out, err
		}
		hr := comCall(e.mft, idxMFTProcessInput, 0, sample, 0)
		comRelease(sample)
		if hr != sOK {
			return out, hrError(hr, "ProcessInput(async)")
		}
		e.pendingIn--
	}
	// 3. Забираем готовый выход без блокировки (что требует следующего кадра —
	//    придёт на следующем вызове).
	for {
		mt, ok, err := e.nextEvent(false)
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}
		if aus, err := e.onEvent(mt); err != nil {
			return out, err
		} else {
			out = append(out, aus...)
		}
	}
	return out, nil
}

// onEvent реагирует на тип события: NeedInput копит счётчик, HaveOutput тянет
// готовый access unit.
func (e *h264Encoder) onEvent(mt uint32) ([][]byte, error) {
	switch mt {
	case meTransformNeedInput:
		e.pendingIn++
		return nil, nil
	case meTransformHaveOutput:
		return e.drainOneOutput()
	}
	return nil, nil
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
