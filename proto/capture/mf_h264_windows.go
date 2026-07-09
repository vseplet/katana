//go:build windows

package capture

// H264-энкодер на Media Foundation H264 Encoder MFT (CLSID_CMSH264EncoderMFT),
// напрямую через IMFTransform (без SinkWriter). Вход — NV12 в системной памяти,
// выход — H264 access unit'ы в Annex-B (со старт-кодами и in-band SPS/PPS на
// keyframe), которые уходят прямо в WebRTC-трек. Динамика (форс IDR, смена
// битрейта) — через ICodecAPI.
//
// ВНИМАНИЕ (проверить на железе): наличие софтового H264 MFT на ARM64-Windows не
// гарантировано. Если CoCreateInstance энкодера падает, videoProbe() вернёт false
// и хост поднимется headless.

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")

	mfplat                   = windows.NewLazySystemDLL("mfplat.dll")
	procMFStartup            = mfplat.NewProc("MFStartup")
	procMFShutdown           = mfplat.NewProc("MFShutdown")
	procMFCreateMediaType    = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSample       = mfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer = mfplat.NewProc("MFCreateMemoryBuffer")
	procMFTEnumEx            = mfplat.NewProc("MFTEnumEx")
)

// --- Диагностика: перечень доступных H264-энкодеров MFT (hardware/software) ------

var (
	catVideoEncoder = mustGUID("{F79EAC7D-E545-4387-BDEE-D647D7BDE42A}") // MFT_CATEGORY_VIDEO_ENCODER
	mftFriendlyName = mustGUID("{314FFBAE-5B41-4C95-9C19-4E7D586FACE3}") // MFT_FRIENDLY_NAME_Attribute
)

// mftRegisterTypeInfo — MFT_REGISTER_TYPE_INFO {major GUID; sub GUID} (32 байта).
type mftRegisterTypeInfo struct {
	major windows.GUID
	sub   windows.GUID
}

// logEncoders логирует доступные H264-энкодеры MFT: отдельно аппаратные (flag
// HARDWARE) и все. Показывает, есть ли на этом (в т.ч. виртуальном) GPU аппаратный
// путь, который позволил бы кодировать прямо из WGC-текстуры без CPU-конвертации.
func logEncoders() {
	out := &mftRegisterTypeInfo{major: mfMediaTypeVideo, sub: mfVideoFormatH264}

	for _, f := range []struct {
		name string
		flag uintptr
	}{{"hardware", 0x4}, {"all", 0x77}} { // HARDWARE ; SYNC|ASYNC|HW|LOCAL|TRANSCODE|SORT
		ppBase := new(uintptr)
		pCount := new(uint32)
		// GUID категории — по значению; ABI-раскладка в callMFTEnumEx (x64 vs arm64).
		hr := callMFTEnumEx(&catVideoEncoder, f.flag, 0,
			uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(ppBase)), uintptr(unsafe.Pointer(pCount)))
		runtime.KeepAlive(out)
		runtime.KeepAlive(ppBase)
		runtime.KeepAlive(pCount)
		if hr != sOK {
			log.Printf("mft-enum(%s): hr=%#x", f.name, uint32(hr))
			continue
		}
		count := int(*pCount)
		base := *ppBase
		log.Printf("mft-enum(%s): найдено %d H264-энкодеров", f.name, count)
		if base != 0 && count > 0 {
			arr := unsafe.Slice((*uintptr)(unsafe.Pointer(base)), count)
			for i := 0; i < count; i++ {
				log.Printf("  [%s %d] %s", f.name, i, activateFriendlyName(arr[i]))
				comRelease(arr[i])
			}
			procCoTaskMemFree.Call(base)
		}
	}
}

// activateFriendlyName вытаскивает дружественное имя MFT из IMFActivate.
func activateFriendlyName(act uintptr) string {
	pStr := new(uintptr)
	pLen := new(uint32)
	hr := comCall(act, 13, uintptr(unsafe.Pointer(&mftFriendlyName)), // IMFAttributes::GetAllocatedString
		uintptr(unsafe.Pointer(pStr)), uintptr(unsafe.Pointer(pLen)))
	runtime.KeepAlive(pStr)
	runtime.KeepAlive(pLen)
	if hr != sOK || *pStr == 0 {
		return "(без имени)"
	}
	s := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(*pStr)))
	procCoTaskMemFree.Call(*pStr)
	return s
}

// MF GUID-ы (форматы, атрибуты медиатипа, параметры ICodecAPI).
var (
	mfMediaTypeVideo       = mustGUID("{73646976-0000-0010-8000-00AA00389B71}")
	mfVideoFormatH264      = mustGUID("{34363248-0000-0010-8000-00AA00389B71}")
	mfVideoFormatNV12      = mustGUID("{3231564E-0000-0010-8000-00AA00389B71}")
	mfMTMajorType          = mustGUID("{48EBA18E-F8C9-4687-BF11-0A74C9F96A8F}")
	mfMTSubtype            = mustGUID("{F7E34C9A-42E8-4714-B74B-CB29D72C35E5}")
	mfMTAvgBitrate         = mustGUID("{20332624-FB0D-4D9E-BD0D-CBF6786C102E}")
	mfMTFrameSize          = mustGUID("{1652C33D-D6B2-4012-B834-72030849A37D}")
	mfMTFrameRate          = mustGUID("{C459A2E8-3D2C-4E44-B132-FEE5156C7BB0}")
	mfMTPixelAspect        = mustGUID("{C6376A1E-8D0A-4027-BE45-6D9A0AD39BB6}")
	mfMTInterlaceMode      = mustGUID("{E2724BB8-E676-4806-B4B2-A8D6EFB44CCD}")
	mfMTMPEG2Profile       = mustGUID("{AD76A80B-2D5C-4E0B-B375-64E520137036}")
	mfMTMaxKeyframeSpacing = mustGUID("{C16EB52B-73A1-476F-8D62-839D6A020652}") // MF_MT_MAX_KEYFRAME_SPACING

	codecRateControlMode = mustGUID("{1C0608E9-370C-4710-8A58-CB6181C42423}")
	codecMeanBitRate     = mustGUID("{F7222374-2144-4815-B550-A37F8E12EE52}")
	codecMaxBitRate      = mustGUID("{9651EAE4-39B9-4BE1-8CB2-7C8AF5B3B9BC}") // AVEncCommonMaxBitRate
	codecBufferSize      = mustGUID("{0DB96574-B6A4-4C8B-8106-3773DE0310CD}") // AVEncCommonBufferSize (бит)
	codecGOPSize         = mustGUID("{95F31B26-95A4-41AA-9303-246A7FC6EEF1}")
	codecLowLatency      = mustGUID("{9C27891A-ED7A-40E1-88E8-B22727A024EE}")
	codecForceKeyFrame   = mustGUID("{398C1B98-8353-475A-9EF2-8F265D260345}")
	codecBPictureCount   = mustGUID("{8D390AAC-DC5C-4200-B57F-814D04BABAB2}") // AVEncMPVDefaultBPictureCount
	codecQualityVsSpeed  = mustGUID("{98332DF8-03CD-476B-89FA-3F9E442DEC9F}") // AVEncCommonQualityVsSpeed
)

const (
	mfVideoInterlaceProgressive = 2   // MFVideoInterlace_Progressive
	avEncH264ProfileHigh        = 100 // eAVEncH264VProfile_High
	avEncRateControlCBR         = 0   // eAVEncCommonRateControlMode_CBR

	mfVersion     = 0x00020070 // MF_VERSION (MF_SDK_VERSION<<16 | MF_API_VERSION)
	mfStartupLite = 1
	clsctxInProc  = 1 // CLSCTX_INPROC_SERVER

	// HRESULT-ы MFT.
	mfeTransformNeedMoreInput = 0xC00D6D72
	mfeTransformStreamChange  = 0xC00D6D61

	// Сообщения MFT.
	mftMsgFlush          = 0x00000000 // MFT_MESSAGE_COMMAND_FLUSH
	mftMsgSetD3DManager  = 0x00000002 // MFT_MESSAGE_SET_D3D_MANAGER
	mftMsgBeginStreaming = 0x10000000
	mftMsgStartOfStream  = 0x10000002

	// Флаги output stream info.
	mftOutputProvidesSamples = 0x100
	mftOutputCanProvide      = 0x200

	// Индексы vtable.
	idxMTSetUINT32         = 21 // IMFMediaType(IMFAttributes)::SetUINT32
	idxMTSetUINT64         = 22 // SetUINT64
	idxMTSetGUID           = 24 // SetGUID
	idxSampleSetTime       = 36 // IMFSample::SetSampleTime
	idxSampleSetDuration   = 38 // SetSampleDuration
	idxSampleConvertCont   = 41 // ConvertToContiguousBuffer
	idxSampleAddBuffer     = 42 // AddBuffer
	idxBufLock             = 3  // IMFMediaBuffer::Lock
	idxBufUnlock           = 4  // Unlock
	idxBufGetCurLen        = 5  // GetCurrentLength
	idxBufSetCurLen        = 6  // SetCurrentLength
	idxMFTGetOutStreamInfo = 7  // IMFTransform::GetOutputStreamInfo
	idxMFTSetInputType     = 15 // SetInputType
	idxMFTSetOutputType    = 16 // SetOutputType
	idxMFTProcessMessage   = 23 // ProcessMessage
	idxMFTProcessInput     = 24 // ProcessInput
	idxMFTProcessOutput    = 25 // ProcessOutput
	idxCodecSetValue       = 9  // ICodecAPI::SetValue
)

// variant — усечённый VARIANT (24 байта на 64-бит), достаточно для VT_UI4/VT_BOOL.
type variant struct {
	vt         uint16
	r1, r2, r3 uint16
	val        uint64
	_          uint64
}

func variantU32(v uint32) variant { return variant{vt: 19, val: uint64(v)} } // VT_UI4
func variantBool(b bool) variant {
	if b {
		return variant{vt: 11, val: 0xFFFF} // VT_BOOL, VARIANT_TRUE
	}
	return variant{vt: 11}
}

// mftOutputDataBuffer — MFT_OUTPUT_DATA_BUFFER (32 байта на 64-бит).
type mftOutputDataBuffer struct {
	dwStreamID uint32
	_          uint32
	pSample    uintptr
	dwStatus   uint32
	_          uint32
	pEvents    uintptr
}

// mftOutputStreamInfo — MFT_OUTPUT_STREAM_INFO.
type mftOutputStreamInfo struct {
	dwFlags     uint32
	cbSize      uint32
	cbAlignment uint32
}

// h264Encoder — обёртка над H264 MFT (софтовый sync или аппаратный async).
type h264Encoder struct {
	mft   uintptr
	codec uintptr // ICodecAPI (может быть 0, если не поддержан)

	// Аппаратный MFT — асинхронный: событийная модель (NeedInput/HaveOutput) через
	// IMFMediaEventGenerator; activate держим ради ShutdownObject в Close.
	async    bool
	evGen    uintptr // IMFMediaEventGenerator (async)
	activate uintptr // IMFActivate, создавший mft (async)
	d3dMgr   uintptr // IMFDXGIDeviceManager, отданный MFT (async, D3D-aware); держим до Close
	// Прокачка async-MFT идёт в ОТДЕЛЬНОЙ горутине (pump): захват только пишет кадры
	// в inCh и забирает готовые AU из outCh, НИКОГДА не блокируясь на событиях
	// энкодера. Иначе тяжёлый кадр (keyframe) стопорил бы поток захвата → микрофриз.
	inCh     chan []byte   // NV12-кадры (копии) → энкодер
	outCh    chan []byte   // готовые H264 access unit'ы ← энкодер
	pumpStop chan struct{} // сигнал остановки pump
	pumpGone chan struct{} // закрывается, когда pump вышел (для Close)

	// Диагностика async-конвейера (atomic): сколько событий получил pump. Помогает
	// понять, почему HW молчит: NeedInput=0 → события не поллятся; NeedInput>0,
	// HaveOutput=0 → кормим, но выхода нет (вероятно, нужен D3D-вход).
	dbgNeedIn  int64
	dbgHaveOut int64

	w, h      int
	fps       int
	outSize   uint32 // размер выходного буфера (cbSize из GetOutputStreamInfo)
	selfAlloc bool   // MFT сам аллоцирует выходные сэмплы

	frame   int64 // счётчик кадров (для таймстампов)
	lastKey int64 // номер кадра последнего кейфрейма (для замера GOP)

	mu       sync.Mutex
	forceKey bool
}

// auHasKeyframe — есть ли в Annex-B access unit'е NAL типа IDR(5) или SPS(7),
// т.е. является ли кадр кейфреймом. Для замера реального интервала кейфреймов.
func auHasKeyframe(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] != 0 || au[i+1] != 0 {
			continue
		}
		var nalStart int
		switch {
		case au[i+2] == 1:
			nalStart = i + 3
		case au[i+2] == 0 && au[i+3] == 1:
			nalStart = i + 4
		default:
			continue
		}
		if nalStart < len(au) {
			if t := au[nalStart] & 0x1f; t == 5 || t == 7 {
				return true
			}
		}
	}
	return false
}

func setGUID(mt uintptr, key *windows.GUID, val *windows.GUID) uintptr {
	return comCall(mt, idxMTSetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(val)))
}
func setU32(mt uintptr, key *windows.GUID, v uint32) uintptr {
	return comCall(mt, idxMTSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v))
}
func setU64(mt uintptr, key *windows.GUID, v uint64) uintptr {
	return comCall(mt, idxMTSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(v))
}

// packU64 упаковывает пару 32-бит в UINT64 (hi<<32 | lo) — формат MF_MT_FRAME_SIZE/RATE.
func packU64(hi, lo uint32) uint64 { return uint64(hi)<<32 | uint64(lo) }

// newH264Encoder создаёт и настраивает H264 MFT под кадры w×h @ fps, битрейт kbps.
// По умолчанию пробуем АППАРАТНЫЙ энкодер (NVENC/QuickSync/AMF) — он кодирует на
// GPU и не выжирает CPU; софтовый CMSH264EncoderMFT на физической машине в полном
// разрешении периодически не влезал в бюджет кадра (overload на движении). Если
// аппаратного нет / не поднялся — тихо откатываемся на софтовый (без регрессии).
// KATANA_SW_ENCODER=1 форсит софтовый (аварийный выход, если HW капризничает).
func newH264Encoder(dev uintptr, w, h, fps, kbps int) (*h264Encoder, error) {
	if hr, _, _ := procMFStartup.Call(mfVersion, mfStartupLite); hr != sOK {
		return nil, hrError(hr, "MFStartup")
	}
	if os.Getenv("KATANA_SW_ENCODER") == "" {
		if e, err := newHardwareH264Encoder(dev, w, h, fps, kbps); err == nil {
			return e, nil
		} else {
			log.Printf("mf: аппаратный H264 недоступен (%v) — софтовый MFT", err)
		}
	}
	hr, mft := procCallOut(procCoCreateInstance,
		uintptr(unsafe.Pointer(&clsidH264Encoder)), 0, clsctxInProc,
		uintptr(unsafe.Pointer(&iidIMFTransform)))
	if err := hrError(hr, "CoCreateInstance(H264 encoder MFT)"); err != nil {
		procMFShutdown.Call()
		return nil, err
	}
	e := &h264Encoder{mft: mft, w: w, h: h, fps: fps}
	if err := e.configure(kbps); err != nil {
		e.Close()
		return nil, err
	}
	log.Printf("mf: софтовый H264 MFT (CMSH264EncoderMFT) %dx%d @ %dfps %dkbps", w, h, fps, kbps)
	return e, nil
}

// configure навешивает на e.mft параметры кодирования (ICodecAPI: CBR/битрейт/VBV/
// GOP/low-latency), входной/выходной медиатипы и запускает стриминг. Общая для
// софтового (sync) и аппаратного (async) MFT — раскладка вызовов IMFTransform
// одинакова. Ошибку возвращаем наверх; освобождение — на вызывающем (e.Close()).
func (e *h264Encoder) configure(kbps int) error {
	w, h, fps := e.w, e.h, e.fps
	bitrate := uint32(kbps * 1000)

	// ICodecAPI задаём ДО SetOutputType — иначе CMSH264EncoderMFT возвращает S_OK,
	// но игнорирует часть свойств (проверено: GOP через ICodecAPI и через media-type
	// после SetOutputType не применялись → кейфрейм каждую 1с). Best-effort, логируем.
	if codec, err := comQueryInterface(e.mft, &iidICodecAPI); err == nil {
		e.codec = codec
		rc := e.codecSet(&codecRateControlMode, variantU32(avEncRateControlCBR))
		br := e.codecSet(&codecMeanBitRate, variantU32(bitrate))
		// VBV/HRD-потолок. Без него CBR позволяет одному P-кадру на резком движении
		// (перетаскивание окна = full-frame motion) распухнуть в разы выше среднего.
		// Над WAN (RTT ~200мс) этот бёрст забивает очередь канала → bufferbloat →
		// latency-спайк 1–2с. BufferSize (leaky-bucket, в БИТАХ) ограничивает, сколько
		// кадр «занимает» из буфера — ставим ~0.5с, паритет с Linux (-maxrate/-bufsize).
		// MaxBitRate — best-effort (учитывается в PeakConstrainedVBR; в CBR обычно игнор).
		mx := e.codecSet(&codecMaxBitRate, variantU32(bitrate))
		buf := e.codecSet(&codecBufferSize, variantU32(bitrate/2))
		// Длинный GOP: кейфреймы по запросу (PLI → ForceKeyframe), а не раз в секунду.
		gop := e.codecSet(&codecGOPSize, variantU32(uint32(fps*10)))
		ll := e.codecSet(&codecLowLatency, variantBool(true))
		bp := e.codecSet(&codecBPictureCount, variantU32(0))
		// Баланс качество/скорость: 100 давал encode ~30мс (не влезает в 16мс на
		// 60fps → overload у софта), 0 — грязно. 25 — умеренное качество в бюджете.
		qs := e.codecSet(&codecQualityVsSpeed, variantU32(25))
		if debugCapture() {
			log.Printf("mf/codec: ICodecAPI — RateControl=%#x MeanBitRate=%#x MaxBitRate=%#x BufSize=%#x GOP=%#x LowLat=%#x BPic=%#x Quality=%#x",
				uint32(rc), uint32(br), uint32(mx), uint32(buf), uint32(gop), uint32(ll), uint32(bp), uint32(qs))
		}
	} else {
		log.Printf("mf/codec: ICodecAPI НЕ получен (%v) — CBR/GOP/качество НЕ настроены, энкодер в дефолте", err)
	}

	// Выходной тип (H264) задаём ПЕРВЫМ — так требует MS-энкодер.
	hrOut, outType := procCallOut(procMFCreateMediaType)
	if hrOut != sOK {
		return hrError(hrOut, "MFCreateMediaType(out)")
	}
	setGUID(outType, &mfMTMajorType, &mfMediaTypeVideo)
	setGUID(outType, &mfMTSubtype, &mfVideoFormatH264)
	setU32(outType, &mfMTAvgBitrate, bitrate)
	setU32(outType, &mfMTInterlaceMode, mfVideoInterlaceProgressive)
	setU64(outType, &mfMTFrameSize, packU64(uint32(w), uint32(h)))
	setU64(outType, &mfMTFrameRate, packU64(uint32(fps), 1))
	setU64(outType, &mfMTPixelAspect, packU64(1, 1))
	setU32(outType, &mfMTMPEG2Profile, avEncH264ProfileHigh)
	// Интервал кейфреймов через атрибут медиатипа (ICodecAPI GOPSize этот MFT
	// игнорирует). Длинный GOP: I-кадр только по PLI + safety-net раз в ~10с,
	// иначе секундные I-кадры при CBR выжирают бюджет → дыхание качества.
	setU32(outType, &mfMTMaxKeyframeSpacing, uint32(fps*10))
	if hr := comCall(e.mft, idxMFTSetOutputType, 0, outType, 0); hr != sOK {
		comRelease(outType)
		return hrError(hr, "SetOutputType")
	}
	comRelease(outType)

	// Входной тип (NV12).
	hrIn, inType := procCallOut(procMFCreateMediaType)
	if hrIn != sOK {
		return hrError(hrIn, "MFCreateMediaType(in)")
	}
	setGUID(inType, &mfMTMajorType, &mfMediaTypeVideo)
	setGUID(inType, &mfMTSubtype, &mfVideoFormatNV12)
	setU32(inType, &mfMTInterlaceMode, mfVideoInterlaceProgressive)
	setU64(inType, &mfMTFrameSize, packU64(uint32(w), uint32(h)))
	setU64(inType, &mfMTFrameRate, packU64(uint32(fps), 1))
	setU64(inType, &mfMTPixelAspect, packU64(1, 1))
	if hr := comCall(e.mft, idxMFTSetInputType, 0, inType, 0); hr != sOK {
		comRelease(inType)
		return hrError(hr, "SetInputType")
	}
	comRelease(inType)

	// Узнаём размер выходного буфера и кто его аллоцирует.
	info := new(mftOutputStreamInfo)
	comCall(e.mft, idxMFTGetOutStreamInfo, 0, uintptr(unsafe.Pointer(info)))
	runtime.KeepAlive(info)
	e.outSize = info.cbSize
	if e.outSize == 0 {
		e.outSize = uint32(w*h) * 2 // запас, если MFT не сообщил
	}
	e.selfAlloc = info.dwFlags&(mftOutputProvidesSamples|mftOutputCanProvide) != 0

	// FLUSH → BEGIN_STREAMING → START_OF_STREAM (порядок как в рабочих примерах).
	// HRESULT'ы логируем: если стриминг не стартовал, async-MFT не шлёт событий —
	// раньше мы это молча проглатывали и не понимали, почему NeedInput=0.
	hrFlush := comCall(e.mft, idxMFTProcessMessage, mftMsgFlush, 0)
	hrBegin := comCall(e.mft, idxMFTProcessMessage, mftMsgBeginStreaming, 0)
	hrStart := comCall(e.mft, idxMFTProcessMessage, mftMsgStartOfStream, 0)
	if e.async || debugCapture() {
		log.Printf("mf: stream msgs — flush=0x%08x begin=0x%08x start=0x%08x",
			uint32(hrFlush), uint32(hrBegin), uint32(hrStart))
	}
	return nil
}

func (e *h264Encoder) codecSet(api *windows.GUID, v variant) uintptr {
	if e.codec == 0 {
		return 0x80004005 // E_FAIL
	}
	pv := new(variant) // входная структура на куче — адрес стабилен на время вызова
	*pv = v
	hr := comCall(e.codec, idxCodecSetValue, uintptr(unsafe.Pointer(api)), uintptr(unsafe.Pointer(pv)))
	runtime.KeepAlive(api)
	runtime.KeepAlive(pv)
	return hr
}

// forceKeyframe помечает следующий кадр как принудительный IDR.
func (e *h264Encoder) forceKeyframe() {
	e.mu.Lock()
	e.forceKey = true
	e.mu.Unlock()
}

// setBitrate меняет средний битрейт энкодера на лету (kbps).
func (e *h264Encoder) setBitrate(kbps int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	bitrate := uint32(kbps * 1000)
	e.codecSet(&codecMeanBitRate, variantU32(bitrate))
	// Держим VBV-буфер пропорциональным (~0.5с), иначе на низком битрейте фиксированный
	// буфер = большее «время» = снова допускает бёрсты.
	e.codecSet(&codecMaxBitRate, variantU32(bitrate))
	e.codecSet(&codecBufferSize, variantU32(bitrate/2))
}

// encode кодирует один NV12-кадр и возвращает получившиеся H264 access unit'ы
// (Annex-B). Обычно 0 или 1 юнит на кадр.
func (e *h264Encoder) encode(nv12 []byte) ([][]byte, error) {
	e.mu.Lock()
	force := e.forceKey
	e.forceKey = false
	e.mu.Unlock()
	if force {
		e.codecSet(&codecForceKeyFrame, variantU32(1))
		if debugCapture() {
			log.Printf("mf/codec: force keyframe (PLI от зрителя)")
		}
	}

	if e.async {
		return e.encodeAsync(nv12)
	}

	sample, err := e.makeInputSample(nv12)
	if err != nil {
		return nil, err
	}
	hr := comCall(e.mft, idxMFTProcessInput, 0, sample, 0)
	comRelease(sample)
	if hr != sOK {
		return nil, hrError(hr, "ProcessInput")
	}
	aus, derr := e.drainOutput()
	if debugCapture() {
		for _, au := range aus {
			if auHasKeyframe(au) {
				log.Printf("mf/codec: keyframe @ кадр %d (интервал %d кадров ~%.1fs, размер %d Б)",
					e.frame, e.frame-e.lastKey, float64(e.frame-e.lastKey)/float64(e.fps), len(au))
				e.lastKey = e.frame
				break
			}
		}
	}
	return aus, derr
}

// makeInputSample оборачивает NV12-байты в IMFSample с таймстампом/длительностью.
func (e *h264Encoder) makeInputSample(nv12 []byte) (uintptr, error) {
	hr, buf := procCallOut(procMFCreateMemoryBuffer, uintptr(len(nv12)))
	if hr != sOK {
		return 0, hrError(hr, "MFCreateMemoryBuffer")
	}
	pDst := new(uintptr)
	pMax, pCur := new(uint32), new(uint32)
	if hr := comCall(buf, idxBufLock, uintptr(unsafe.Pointer(pDst)),
		uintptr(unsafe.Pointer(pMax)), uintptr(unsafe.Pointer(pCur))); hr != sOK {
		comRelease(buf)
		return 0, hrError(hr, "buffer Lock")
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(*pDst)), len(nv12)), nv12)
	runtime.KeepAlive(pDst)
	runtime.KeepAlive(pMax)
	runtime.KeepAlive(pCur)
	comCall(buf, idxBufUnlock)
	comCall(buf, idxBufSetCurLen, uintptr(len(nv12)))

	hrs, sample := procCallOut(procMFCreateSample)
	if hrs != sOK {
		comRelease(buf)
		return 0, hrError(hrs, "MFCreateSample")
	}
	comCall(sample, idxSampleAddBuffer, buf)
	comRelease(buf) // сэмпл держит свою ссылку

	dur := int64(10_000_000) / int64(e.fps)
	t := e.frame * dur
	e.frame++
	comCall(sample, idxSampleSetTime, uintptr(t))
	comCall(sample, idxSampleSetDuration, uintptr(dur))
	return sample, nil
}

// drainOutput вытягивает все готовые выходные сэмплы до NEED_MORE_INPUT.
func (e *h264Encoder) drainOutput() ([][]byte, error) {
	var out [][]byte
	for {
		var outSample uintptr
		if !e.selfAlloc {
			s, err := e.makeOutputSample()
			if err != nil {
				return out, err
			}
			outSample = s
		}
		odb := new(mftOutputDataBuffer) // out-структура на куче
		odb.pSample = outSample
		status := new(uint32)
		hr := comCall(e.mft, idxMFTProcessOutput, 0, 1,
			uintptr(unsafe.Pointer(odb)), uintptr(unsafe.Pointer(status)))
		runtime.KeepAlive(odb)
		runtime.KeepAlive(status)

		if uint32(hr) == mfeTransformNeedMoreInput {
			if outSample != 0 {
				comRelease(outSample)
			}
			return out, nil
		}
		if uint32(hr) == mfeTransformStreamChange {
			if outSample != 0 {
				comRelease(outSample)
			}
			return out, nil // смена формата на лету не обрабатываем — ждём след. кадр
		}
		if hr != sOK {
			if outSample != 0 {
				comRelease(outSample)
			}
			return out, hrError(hr, "ProcessOutput")
		}

		produced := odb.pSample // при selfAlloc MFT вернёт свой сэмпл сюда
		if au := e.readSample(produced); au != nil {
			out = append(out, au)
		}
		comRelease(produced)
	}
}

// makeOutputSample аллоцирует пустой выходной IMFSample с буфером outSize.
func (e *h264Encoder) makeOutputSample() (uintptr, error) {
	hr, buf := procCallOut(procMFCreateMemoryBuffer, uintptr(e.outSize))
	if hr != sOK {
		return 0, hrError(hr, "MFCreateMemoryBuffer(out)")
	}
	hrs, sample := procCallOut(procMFCreateSample)
	if hrs != sOK {
		comRelease(buf)
		return 0, hrError(hrs, "MFCreateSample(out)")
	}
	comCall(sample, idxSampleAddBuffer, buf)
	comRelease(buf)
	return sample, nil
}

// readSample вытягивает байты access unit'а из сэмпла и приводит к Annex-B.
func (e *h264Encoder) readSample(sample uintptr) []byte {
	if sample == 0 {
		return nil
	}
	hr, buf := comCallOut(sample, idxSampleConvertCont)
	if hr != sOK || buf == 0 {
		return nil
	}
	defer comRelease(buf)
	pData := new(uintptr)
	pMax, pCur := new(uint32), new(uint32)
	if hr := comCall(buf, idxBufLock, uintptr(unsafe.Pointer(pData)),
		uintptr(unsafe.Pointer(pMax)), uintptr(unsafe.Pointer(pCur))); hr != sOK {
		return nil
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(*pData)), int(*pCur))
	au := ensureAnnexB(src)
	runtime.KeepAlive(pData)
	runtime.KeepAlive(pCur)
	runtime.KeepAlive(pMax)
	comCall(buf, idxBufUnlock)
	return au
}

// ensureAnnexB возвращает копию access unit'а в Annex-B. MS-энкодер по умолчанию
// уже отдаёт byte-stream со старт-кодами; если же поток length-prefixed (AVCC,
// 4-байтная длина), переписываем в старт-коды.
func ensureAnnexB(src []byte) []byte {
	if len(src) < 4 {
		return append([]byte(nil), src...)
	}
	if src[0] == 0 && src[1] == 0 && ((src[2] == 0 && src[3] == 1) || src[2] == 1) {
		return append([]byte(nil), src...) // уже Annex-B
	}
	// Пробуем как AVCC (4-байтные длины). Если раскладка не бьётся — отдаём как есть.
	var out []byte
	start := []byte{0, 0, 0, 1}
	i := 0
	for i+4 <= len(src) {
		n := int(binary.BigEndian.Uint32(src[i:]))
		i += 4
		if n <= 0 || i+n > len(src) {
			return append([]byte(nil), src...)
		}
		out = append(out, start...)
		out = append(out, src[i:i+n]...)
		i += n
	}
	if i != len(src) || len(out) == 0 {
		return append([]byte(nil), src...)
	}
	return out
}

// Close освобождает MFT и завершает Media Foundation.
func (e *h264Encoder) Close() {
	if e == nil {
		return
	}
	// Остановить прокачивающую горутину ДО релиза COM (иначе pump дёрнет
	// освобождённый MFT). stopPump будит блокирующий GetEvent и ждёт выхода pump.
	e.stopPump()
	if e.d3dMgr != 0 {
		comRelease(e.d3dMgr)
		e.d3dMgr = 0
	}
	if e.evGen != 0 {
		comRelease(e.evGen)
		e.evGen = 0
	}
	if e.codec != 0 {
		comRelease(e.codec)
		e.codec = 0
	}
	if e.mft != 0 {
		comRelease(e.mft)
		e.mft = 0
	}
	// Аппаратный объект: гасим через IMFActivate::ShutdownObject, затем освобождаем.
	if e.activate != 0 {
		comCall(e.activate, idxActivateShutdownObject)
		comRelease(e.activate)
		e.activate = 0
	}
	procMFShutdown.Call()
}

// probeH264Encoder — быстрая проверка, что H264 MFT вообще создаётся в этом
// рантайме (для VideoAvailable). Возвращает ошибку с причиной.
func probeH264Encoder() error {
	if hr, _, _ := procMFStartup.Call(mfVersion, mfStartupLite); hr != sOK {
		return hrError(hr, "MFStartup")
	}
	defer procMFShutdown.Call()
	hr, mft := procCallOut(procCoCreateInstance,
		uintptr(unsafe.Pointer(&clsidH264Encoder)), 0, clsctxInProc,
		uintptr(unsafe.Pointer(&iidIMFTransform)))
	if err := hrError(hr, "CoCreateInstance(H264 encoder MFT)"); err != nil {
		return fmt.Errorf("нет H264 encoder MFT: %w", err)
	}
	comRelease(mft)
	return nil
}
