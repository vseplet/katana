// Нативный аппаратный H264-энкодер на Media Foundation (async hardware MFT),
// делящий D3D11-устройство с WGC-захватом. См. mf_encoder_windows.h.
//
// Компилируется только под Windows (суффикс _windows.c) и только когда включён cgo
// (тег winnative). Вся COM-асинхронщина (NeedInput/HaveOutput через IMFAsyncCallback)
// живёт здесь; Go кладёт NV12 и забирает Annex-B.

#include <windows.h>
#include <d3d11.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mferror.h>
#include <codecapi.h>
#include <strmif.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>

#include "mf_encoder_windows.h"

// Типы событий асинхронного MFT (на случай, если тулчейн их не объявил).
#ifndef METransformNeedInput
#define METransformNeedInput 601
#endif
#ifndef METransformHaveOutput
#define METransformHaveOutput 602
#endif

// Пул NV12-текстур для zero-copy: энкодер держит входной сэмпл до обработки, поэтому
// нельзя перезаписывать текстуру сразу — крутим по кругу (in_len дропается до 2).
#define KATANA_NV12_POOL 4

// --- очередь кадров (односвязный список) ---
// Вход: либо копия NV12-байтов (data, байтовый путь), либо готовый D3D-сэмпл
// (sample, zero-copy путь). Выход: всегда data (Annex-B).
typedef struct frame_node {
    struct frame_node *next;
    uint8_t *data;
    int len;
    IMFSample *sample; // zero-copy: готовый входной сэмпл (иначе NULL)
} frame_node;

typedef struct {
    IMFAsyncCallbackVtbl *lpVtbl;
    LONG ref;
    katana_enc *enc;
} async_cb;

struct katana_enc {
    IMFTransform *mft;
    IMFMediaEventGenerator *evgen;
    IMFDXGIDeviceManager *devmgr;
    struct ICodecAPI *codec; // для форса ключевого кадра по PLI (может быть NULL)
    async_cb cb;

    CRITICAL_SECTION lock;
    frame_node *in_head, *in_tail;
    frame_node *out_head, *out_tail;
    int in_len;     // длина входной очереди (для дропа при отставании энкодера)
    int out_len;    // длина выходной очереди (для дропа при заторе пайпа/сети)
    int need_input; // сколько NeedInput пришло, пока очередь входа была пуста

    LONGLONG sample_time; // 100-нс тики
    LONGLONG frame_dur;
    int width, height, fps;
    UINT reset_token;
    volatile LONG dead;
    int intra_refresh; // 1 если AMD принял rolling intra-refresh (проверка по GetValue)

    // --- zero-copy: GPU-конверт BGRA→NV12 через ID3D11VideoProcessor на общем девайсе ---
    // Девайс с MT-protection (ID3D11Multithread) сам сериализует контекст, поэтому
    // LockDevice не нужен. Кадр WGC копируем в свою BGRA-текстуру (CopyResource), VP
    // конвертит+масштабирует в NV12-текстуру из пула, оборачиваем в IMFSample.
    ID3D11Device *d3d;
    ID3D11DeviceContext *d3dctx;
    ID3D11VideoDevice *vdev;
    ID3D11VideoContext *vctx;
    ID3D11VideoProcessorEnumerator *vpenum;
    ID3D11VideoProcessor *vp;
    ID3D11Texture2D *bgra_in;                 // владеемая копия кадра (вход VP)
    ID3D11VideoProcessorInputView *bgra_view; // input view на bgra_in (создаётся раз)
    ID3D11Texture2D *nv12tex[KATANA_NV12_POOL];
    ID3D11VideoProcessorOutputView *nv12view[KATANA_NV12_POOL];
    int nv12_idx;   // round-robin по пулу
    int src_w, src_h;
    int vp_ready;   // 1 = video processor поднят, zero-copy активен
};

// ---- очередь: push/pop под внешним локом ----
static void q_push(frame_node **head, frame_node **tail, uint8_t *data, int len) {
    frame_node *n = (frame_node *)malloc(sizeof(frame_node));
    if (!n) { free(data); return; }
    n->next = NULL; n->data = data; n->len = len; n->sample = NULL;
    if (*tail) (*tail)->next = n; else *head = n;
    *tail = n;
}
// q_push_sample — вариант для zero-copy: кладём готовый входной IMFSample.
static void q_push_sample(frame_node **head, frame_node **tail, IMFSample *s) {
    frame_node *n = (frame_node *)malloc(sizeof(frame_node));
    if (!n) { if (s) IMFSample_Release(s); return; }
    n->next = NULL; n->data = NULL; n->len = 0; n->sample = s;
    if (*tail) (*tail)->next = n; else *head = n;
    *tail = n;
}
// node_free освобождает узел входной очереди (байты ИЛИ D3D-сэмпл).
static void node_free(frame_node *n) {
    if (!n) return;
    if (n->data) free(n->data);
    if (n->sample) IMFSample_Release(n->sample);
    free(n);
}
static frame_node *q_pop(frame_node **head, frame_node **tail) {
    frame_node *n = *head;
    if (!n) return NULL;
    *head = n->next;
    if (!*head) *tail = NULL;
    n->next = NULL;
    return n;
}

// ---- IMFAsyncCallback vtable ----
static HRESULT STDMETHODCALLTYPE cb_QueryInterface(IMFAsyncCallback *self, REFIID riid, void **ppv) {
    if (IsEqualIID(riid, &IID_IUnknown) || IsEqualIID(riid, &IID_IMFAsyncCallback)) {
        *ppv = self;
        IMFAsyncCallback_AddRef(self);
        return S_OK;
    }
    *ppv = NULL;
    return E_NOINTERFACE;
}
static ULONG STDMETHODCALLTYPE cb_AddRef(IMFAsyncCallback *self) {
    async_cb *c = (async_cb *)self;
    return InterlockedIncrement(&c->ref);
}
static ULONG STDMETHODCALLTYPE cb_Release(IMFAsyncCallback *self) {
    async_cb *c = (async_cb *)self;
    return InterlockedDecrement(&c->ref);
}
static HRESULT STDMETHODCALLTYPE cb_GetParameters(IMFAsyncCallback *self, DWORD *flags, DWORD *queue) {
    (void)self;
    *flags = 0; *queue = 0;
    return S_OK;
}

// forward
static void enc_feed_locked(katana_enc *e);
static void enc_drain_output(katana_enc *e);

static HRESULT STDMETHODCALLTYPE cb_Invoke(IMFAsyncCallback *self, IMFAsyncResult *result) {
    async_cb *c = (async_cb *)self;
    katana_enc *e = c->enc;
    if (!e || e->dead) return S_OK;

    IMFMediaEvent *ev = NULL;
    HRESULT hr = IMFMediaEventGenerator_EndGetEvent(e->evgen, result, &ev);
    if (SUCCEEDED(hr) && ev) {
        MediaEventType met = 0;
        IMFMediaEvent_GetType(ev, &met);
        if (met == METransformNeedInput) {
            EnterCriticalSection(&e->lock);
            e->need_input++;
            enc_feed_locked(e);
            LeaveCriticalSection(&e->lock);
        } else if (met == METransformHaveOutput) {
            enc_drain_output(e);
        }
        IMFMediaEvent_Release(ev);
    }
    // Пере-вооружаемся на следующее событие.
    if (!e->dead) {
        IMFMediaEventGenerator_BeginGetEvent(e->evgen, (IMFAsyncCallback *)&e->cb, NULL);
    }
    return S_OK;
}

static IMFAsyncCallbackVtbl g_cb_vtbl = {
    cb_QueryInterface, cb_AddRef, cb_Release, cb_GetParameters, cb_Invoke,
};

// Порог входной очереди: при отставании энкодера дропаем самые старые кадры, чтобы
// латенси не раздувалось (низкая задержка важнее полноты — как dropLate у ffmpeg).
#define KATANA_IN_QUEUE_MAX 2
// Порог выходной очереди: если пайп/сеть затыкаются (медленный зритель, TURN/TCP),
// out-очередь не должна расти безгранично — держим только свежие AU, старые дропаем.
#define KATANA_OUT_QUEUE_MAX 8

// ---- скармливание входа: под e->lock, пока есть и запросы, и кадры ----
static void enc_feed_locked(katana_enc *e) {
    while (e->need_input > 0 && e->in_head) {
        frame_node *n = q_pop(&e->in_head, &e->in_tail);
        if (!n) break;
        e->in_len--;

        IMFSample *sample = n->sample; // zero-copy: готовый D3D-сэмпл
        IMFMediaBuffer *buf = NULL;
        int owns_sample = 0; // sample создан здесь (байтовый путь) → релизим сами
        if (!sample) {
            // Байтовый путь (фолбэк): строим сэмпл из NV12-копии.
            if (SUCCEEDED(MFCreateSample(&sample)) &&
                SUCCEEDED(MFCreateMemoryBuffer(n->len, &buf))) {
                owns_sample = 1;
                BYTE *dst = NULL; DWORD maxlen = 0;
                if (SUCCEEDED(IMFMediaBuffer_Lock(buf, &dst, &maxlen, NULL))) {
                    memcpy(dst, n->data, n->len);
                    IMFMediaBuffer_Unlock(buf);
                    IMFMediaBuffer_SetCurrentLength(buf, n->len);
                    IMFSample_AddBuffer(sample, buf);
                } else {
                    if (buf) { IMFMediaBuffer_Release(buf); buf = NULL; }
                    IMFSample_Release(sample); sample = NULL;
                }
            } else {
                if (sample) { IMFSample_Release(sample); sample = NULL; }
            }
        } else {
            owns_sample = 1; // забрали владение из очереди — освободим ниже
            n->sample = NULL;
        }
        if (sample) {
            IMFSample_SetSampleTime(sample, e->sample_time);
            IMFSample_SetSampleDuration(sample, e->frame_dur);
            e->sample_time += e->frame_dur;
            if (SUCCEEDED(IMFTransform_ProcessInput(e->mft, 0, sample, 0)))
                e->need_input--; // квоту тратим только при принятом кадре
        }
        if (buf) IMFMediaBuffer_Release(buf);
        if (sample && owns_sample) IMFSample_Release(sample);
        node_free(n);
    }
}

// ---- вытягивание выхода: копируем Annex-B в out-очередь ----
static void enc_drain_output(katana_enc *e) {
    MFT_OUTPUT_STREAM_INFO si;
    memset(&si, 0, sizeof(si));
    IMFTransform_GetOutputStreamInfo(e->mft, 0, &si);
    int provides = (si.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES | MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES)) != 0;

    IMFSample *osample = NULL;
    IMFMediaBuffer *obuf = NULL;
    if (!provides) {
        if (FAILED(MFCreateSample(&osample))) return;
        DWORD cb = si.cbSize > 0 ? si.cbSize : (DWORD)(e->width * e->height * 3 / 2);
        if (FAILED(MFCreateMemoryBuffer(cb, &obuf))) { IMFSample_Release(osample); return; }
        IMFSample_AddBuffer(osample, obuf);
    }

    MFT_OUTPUT_DATA_BUFFER odb;
    memset(&odb, 0, sizeof(odb));
    odb.dwStreamID = 0;
    odb.pSample = osample; // NULL, если MFT сам аллоцирует
    DWORD status = 0;
    // ProcessOutput НЕ под локом: async MFT гарантирует потокобезопасность одновременных
    // ProcessInput (поток захвата) и ProcessOutput (этот callback-поток). Держать их под
    // одним локом = head-of-line blocking: если ProcessOutput ждёт GPU (общий D3D-девайс
    // с захватом), submit виснет на локе → захват встаёт. ProcessOutput и так сериализован
    // сам с собой — событий HaveOutput всегда одно в полёте (один BeginGetEvent).
    HRESULT hr = IMFTransform_ProcessOutput(e->mft, 0, 1, &odb, &status);
    if (SUCCEEDED(hr) && odb.pSample) {
        IMFMediaBuffer *cbuf = NULL;
        if (SUCCEEDED(IMFSample_ConvertToContiguousBuffer(odb.pSample, &cbuf))) {
            BYTE *p = NULL; DWORD cur = 0;
            if (SUCCEEDED(IMFMediaBuffer_Lock(cbuf, &p, NULL, &cur)) && cur > 0) {
                uint8_t *copy = (uint8_t *)malloc(cur);
                if (copy) {
                    memcpy(copy, p, cur);
                    EnterCriticalSection(&e->lock);
                    q_push(&e->out_head, &e->out_tail, copy, (int)cur);
                    e->out_len++;
                    // Затор пайпа/сети: не копим бэклог, держим только свежие AU.
                    while (e->out_len > KATANA_OUT_QUEUE_MAX) {
                        frame_node *old = q_pop(&e->out_head, &e->out_tail);
                        if (!old) break;
                        e->out_len--;
                        free(old->data);
                        free(old);
                    }
                    LeaveCriticalSection(&e->lock);
                }
                IMFMediaBuffer_Unlock(cbuf);
            }
            IMFMediaBuffer_Release(cbuf);
        }
    }
    // MFT-аллоцированный sample освобождаем; наш переиспользуемый — тоже.
    if (odb.pSample) IMFSample_Release(odb.pSample);
    if (obuf) IMFMediaBuffer_Release(obuf);
    if (odb.pEvents) IMFCollection_Release(odb.pEvents);
}

// Упаковка пары UINT32 в UINT64-атрибут (MF_MT_FRAME_SIZE/RATE/PIXEL_ASPECT_RATIO) —
// заменяет MFSetAttributeSize/Ratio, которых нет в mingw как объявленных функций.
static void attr_u64(IMFMediaType *t, const GUID *key, UINT32 hi, UINT32 lo) {
    IMFAttributes_SetUINT64((IMFAttributes *)t, key, ((UINT64)hi << 32) | (UINT64)lo);
}

// ---- конфигурация типов ----
static HRESULT set_output_type(katana_enc *e, int bitrate_kbps) {
    IMFMediaType *t = NULL;
    HRESULT hr = MFCreateMediaType(&t);
    if (FAILED(hr)) return hr;
    IMFMediaType_SetGUID(t, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(t, &MF_MT_SUBTYPE, &MFVideoFormat_H264);
    IMFMediaType_SetUINT32(t, &MF_MT_AVG_BITRATE, (UINT32)bitrate_kbps * 1000);
    IMFMediaType_SetUINT32(t, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    attr_u64(t, &MF_MT_FRAME_SIZE, e->width, e->height);
    attr_u64(t, &MF_MT_FRAME_RATE, e->fps, 1);
    attr_u64(t, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    // High profile — КАК НА macOS (VideoToolbox ставит H264_High). CABAC + 8x8-трансформ
    // + лучшее интра-предсказание: заметно качественнее Baseline при том же битрейте (та
    // самая «шакальность» на движении). B-кадров нет (BPictureCount=0 ниже + low-latency),
    // так что латенси не растёт. Браузер декодит High поверх SDP profile-level-id=42xxxx —
    // мак уже так работает у того же зрителя. Откат: eAVEncH264VProfile_Base.
    IMFMediaType_SetUINT32(t, &MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_High);
    hr = IMFTransform_SetOutputType(e->mft, 0, t, 0);
    IMFMediaType_Release(t);
    return hr;
}

static HRESULT set_input_type(katana_enc *e) {
    IMFMediaType *t = NULL;
    HRESULT hr = MFCreateMediaType(&t);
    if (FAILED(hr)) return hr;
    IMFMediaType_SetGUID(t, &MF_MT_MAJOR_TYPE, &MFMediaType_Video);
    IMFMediaType_SetGUID(t, &MF_MT_SUBTYPE, &MFVideoFormat_NV12);
    IMFMediaType_SetUINT32(t, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
    attr_u64(t, &MF_MT_FRAME_SIZE, e->width, e->height);
    attr_u64(t, &MF_MT_FRAME_RATE, e->fps, 1);
    attr_u64(t, &MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
    hr = IMFTransform_SetInputType(e->mft, 0, t, 0);
    IMFMediaType_Release(t);
    return hr;
}

// GUID'ы ICodecAPI задаём вручную — codecapi.h в mingw уводит их в C++-ветку
// (__uuidof), поэтому в C они не объявлены. Значения стандартные (Windows SDK).
static const GUID k_IID_ICodecAPI     = {0x901db4c7,0x31ce,0x41a2,{0x85,0xdc,0x8f,0xa0,0xbf,0x41,0xb8,0xda}};
static const GUID k_RateControlMode   = {0x1c0608e9,0x370c,0x4710,{0x8a,0x58,0xcb,0x61,0x81,0xc4,0x24,0x23}};
static const GUID k_MaxBitRate        = {0x9651eae4,0x39b9,0x4be1,{0x8c,0xb2,0x7c,0x8a,0xf5,0xb3,0xb9,0xbc}};
static const GUID k_BufferSize        = {0x0db96574,0xb6a4,0x4c8b,{0x81,0x06,0x37,0x73,0xde,0x03,0x10,0xcd}};
static const GUID k_GOPSize           = {0x95f31b26,0x95a4,0x41aa,{0x93,0x03,0x24,0x6a,0x7f,0xc6,0xee,0xf1}};
static const GUID k_LowLatency        = {0x9d3ecd55,0x89e8,0x490a,{0x97,0x0a,0x0c,0x95,0x48,0xd5,0xa5,0x6e}};
static const GUID k_ForceKeyFrame     = {0x398c1b98,0x8353,0x475a,{0x9e,0xf2,0x8f,0x26,0x5d,0x26,0x03,0x45}};
static const GUID k_MeanBitRate       = {0xf7222374,0x2144,0x4815,{0xb5,0x50,0xa3,0x7f,0x8e,0x12,0xee,0x52}};
// CODECAPI_AVEncMPVDefaultBPictureCount — число B-кадров. Ставим 0: B-кадры добавляют
// реордеринг = латенси, а с High-профилем они разрешены (в Baseline были запрещены).
static const GUID k_BPictureCount     = {0x8d390aac,0xdc5c,0x4200,{0xb5,0x7d,0xdd,0xf3,0x2e,0xd5,0x8a,0x24}};
// Нарезка кадра на слайсы (устойчивость к потерям: 1 потерянный пакет убивает
// один слайс = полоску кадра, а не весь кадр → нет каскада «1 потеря → N кадров»).
static const GUID k_SliceControlMode  = {0xe9e782ef,0x5f18,0x44c9,{0xa9,0x0b,0xe9,0xc3,0xc2,0xc4,0x66,0x98}};
static const GUID k_SliceControlSize  = {0x92f51df3,0x07a5,0x4172,{0xae,0xfe,0xc6,0x9c,0xa3,0xb6,0x0e,0x35}};
// Rolling intra-refresh: постоянно размазываем свежие I-блоки по кадрам. Потеря
// самозалечивается за цикл рефреша БЕЗ ожидания кейфрейма/досылки — декодер продолжает
// (замазывает), а не фризит. Значение = число кадров на полный проход рефреша.
static const GUID k_GradualIntraRefresh = {0x8f347dee,0xcb0d,0x49ba,{0xb4,0x62,0xdb,0x69,0x27,0xee,0x21,0x01}};

// CBR + VBV (буфер ~0.5с) + GOP + low-latency — гасит всплески битрейта на движении
// и ключевых кадрах (иначе на WAN подскакивает латенси). Best-effort: не все
// энкодеры поддерживают всё; неуспех отдельного SetValue не критичен.
static void configure_codecapi(katana_enc *e, int gop, int bitrate_kbps) {
    ICodecAPI *api = NULL;
    if (FAILED(IMFTransform_QueryInterface(e->mft, &k_IID_ICodecAPI, (void **)&api)) || !api)
        return;
    VARIANT v;
    VariantInit(&v);
    // Конфиг КАК НА macOS (VideoToolbox): мягкий средний битрейт (UnconstrainedVBR),
    // БЕЗ жёсткого VBV и без потолка. Мак ставит только AverageBitRate и никаких
    // DataRateLimits — энкодер сам раскидывает биты по сложности кадра, отсюда и
    // качество, и плавность. Прежние строгий CBR + малый VBV душили качество и давали
    // периодический спайк на ключевом кадре.
    v.vt = VT_UI4; v.ulVal = 2; // eAVEncCommonRateControlMode_UnconstrainedVBR
    ICodecAPI_SetValue(api, &k_RateControlMode, &v);
    v.vt = VT_UI4; v.ulVal = (ULONG)bitrate_kbps * 1000;
    ICodecAPI_SetValue(api, &k_MeanBitRate, &v);
    // MaxBitRate/BufferSize НЕ ставим — как на маке (нет жёсткого потолка/буфера).
    if (gop > 0) {
        v.vt = VT_UI4; v.ulVal = (ULONG)gop;
        ICodecAPI_SetValue(api, &k_GOPSize, &v);
    }
    v.vt = VT_BOOL; v.boolVal = VARIANT_TRUE;
    ICodecAPI_SetValue(api, &k_LowLatency, &v);
    // Без B-кадров (High-профиль их разрешает — гасим реордеринг/латенси). Как на маке
    // (AllowFrameReordering=false). Best-effort.
    v.vt = VT_UI4; v.ulVal = 0;
    ICodecAPI_SetValue(api, &k_BPictureCount, &v);
    // Нарезка на слайсы ~по 1 MTU: одна потеря пакета убивает один слайс (полоску),
    // а не весь кадр → нет каскада «1 потеря → N кадров». Mode=1 (лимит по битам на
    // слайс), Size в битах. Часть AMD-драйверов может это игнорировать — проверяем по
    // фактическому числу VCL-NAL в кадре на Go-стороне (лог "native slices/frame").
    v.vt = VT_UI4; v.ulVal = 1;
    ICodecAPI_SetValue(api, &k_SliceControlMode, &v);
    v.vt = VT_UI4; v.ulVal = 10000; // ~1250 байт на слайс
    ICodecAPI_SetValue(api, &k_SliceControlSize, &v);

    // Rolling intra-refresh — устойчивость к потерям БЕЗ буфера/лага (см. GUID выше).
    // Значение = кадров на полный проход (fps ≈ полный рефреш за 1с). Проверяем, взял
    // ли драйвер, через обратный GetValue — как со слайсами AMD может молча игнорить.
    int fps_ir = e->fps > 0 ? e->fps : 30;
    VARIANT vir; VariantInit(&vir);
    vir.vt = VT_UI4; vir.ulVal = (ULONG)fps_ir;
    ICodecAPI_SetValue(api, &k_GradualIntraRefresh, &vir);
    VARIANT vchk; VariantInit(&vchk);
    e->intra_refresh = (SUCCEEDED(ICodecAPI_GetValue(api, &k_GradualIntraRefresh, &vchk))
                        && vchk.vt == VT_UI4 && vchk.ulVal == (ULONG)fps_ir) ? 1 : 0;
    VariantClear(&vchk);

    // Держим ссылку для форса ключевого кадра по PLI; релиз — в destroy.
    e->codec = api;
}

katana_enc *katana_enc_create(void *d3d_device, int width, int height, int fps,
                              int bitrate_kbps, int gop, int32_t *out_hr, int *out_stage,
                              char *out_info, int info_cap) {
    HRESULT hr = S_OK;
    int stage = 0;
    if (out_info && info_cap > 0) out_info[0] = 0;
    katana_enc *e = (katana_enc *)calloc(1, sizeof(katana_enc));
    if (!e) { if (out_hr) *out_hr = E_OUTOFMEMORY; if (out_stage) *out_stage = 0; return NULL; }
    e->width = width; e->height = height; e->fps = fps > 0 ? fps : 30;
    e->frame_dur = 10000000LL / e->fps;
    e->cb.lpVtbl = &g_cb_vtbl;
    e->cb.ref = 1;
    e->cb.enc = e;
    InitializeCriticalSection(&e->lock);

    stage = 1;
    hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
    if (FAILED(hr)) goto fail;

    // Ищем аппаратный H264-энкодер (async hardware MFT).
    stage = 2;
    MFT_REGISTER_TYPE_INFO type_info = { MFMediaType_Video, MFVideoFormat_H264 };
    IMFActivate **acts = NULL;
    UINT32 count = 0;
    hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                   MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_ASYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER,
                   NULL, &type_info, &acts, &count);
    if (FAILED(hr)) goto fail;
    if (count == 0) { stage = 3; hr = MF_E_NOT_FOUND; goto fail; }

    // Диагностика: имя первого кандидата — что винда предлагает как аппаратный H264.
    if (out_info && info_cap > 8) {
        char pfx[32];
        int pn = sprintf(pfx, "count=%u name=", (unsigned)count);
        int off = 0;
        for (int k = 0; k < pn && off < info_cap - 1; k++) out_info[off++] = pfx[k];
        WCHAR *wname = NULL; UINT32 wlen = 0;
        if (SUCCEEDED(IMFActivate_GetAllocatedString(acts[0], &MFT_FRIENDLY_NAME_Attribute, &wname, &wlen)) && wname) {
            WideCharToMultiByte(CP_UTF8, 0, wname, -1, out_info + off, info_cap - off, NULL, NULL);
            CoTaskMemFree(wname);
        } else {
            out_info[off] = 0;
        }
        out_info[info_cap - 1] = 0;
    }

    // Активируем по очереди — первый рабочий берём. Async-режим разблокируем на
    // activate ДО активации (для аппаратных MFT так надёжнее).
    stage = 4;
    hr = MF_E_NOT_FOUND;
    for (UINT32 i = 0; i < count; i++) {
        IMFActivate_SetUINT32((IMFActivate *)acts[i], &MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
        HRESULT ah = IMFActivate_ActivateObject(acts[i], &IID_IMFTransform, (void **)&e->mft);
        if (SUCCEEDED(ah)) { hr = S_OK; break; }
        hr = ah;
    }
    for (UINT32 i = 0; i < count; i++) IMFActivate_Release(acts[i]);
    CoTaskMemFree(acts);
    if (FAILED(hr)) goto fail;

    // Разблокируем async-режим.
    IMFAttributes *attr = NULL;
    if (SUCCEEDED(IMFTransform_GetAttributes(e->mft, &attr)) && attr) {
        IMFAttributes_SetUINT32(attr, &MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
        IMFAttributes_Release(attr);
    }

    // Делим D3D11-устройство с захватом.
    if (d3d_device) {
        // Держим сам девайс и его immediate context для zero-copy VP-конверта.
        e->d3d = (ID3D11Device *)d3d_device;
        ID3D11Device_AddRef(e->d3d);
        ID3D11Device_GetImmediateContext(e->d3d, &e->d3dctx);

        hr = MFCreateDXGIDeviceManager(&e->reset_token, &e->devmgr);
        if (SUCCEEDED(hr)) {
            hr = IMFDXGIDeviceManager_ResetDevice(e->devmgr, (IUnknown *)d3d_device, e->reset_token);
            if (SUCCEEDED(hr)) {
                IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_SET_D3D_MANAGER, (ULONG_PTR)e->devmgr);
            }
        }
        // Не фатально: если не вышло разделить девайс — MFT возьмёт свой.
    }

    // Порядок важен: сначала выходной тип, затем входной.
    stage = 5;
    hr = set_output_type(e, bitrate_kbps);
    if (FAILED(hr)) goto fail;
    stage = 6;
    hr = set_input_type(e);
    if (FAILED(hr)) goto fail;
    configure_codecapi(e, gop, bitrate_kbps);
    if (out_info && info_cap > 0) {
        size_t l = strlen(out_info);
        const char *tag = e->intra_refresh ? " ir=ok" : " ir=IGNORED";
        for (size_t k = 0; tag[k] && l < (size_t)info_cap - 1; k++) out_info[l++] = tag[k];
        out_info[l] = 0;
    }

    stage = 7;
    hr = IMFTransform_QueryInterface(e->mft, &IID_IMFMediaEventGenerator, (void **)&e->evgen);
    if (FAILED(hr)) goto fail;

    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);

    // Запускаем цикл событий.
    stage = 8;
    hr = IMFMediaEventGenerator_BeginGetEvent(e->evgen, (IMFAsyncCallback *)&e->cb, NULL);
    if (FAILED(hr)) goto fail;

    if (out_hr) *out_hr = S_OK;
    if (out_stage) *out_stage = 0;
    return e;

fail:
    if (out_hr) *out_hr = hr;
    if (out_stage) *out_stage = stage;
    katana_enc_destroy(e);
    return NULL;
}

// drop_input_overflow_locked дропает самые старые входные кадры при отставании
// энкодера (низкая задержка важнее полноты). Вызывать под e->lock.
static void drop_input_overflow_locked(katana_enc *e) {
    while (e->in_len > KATANA_IN_QUEUE_MAX) {
        frame_node *old = q_pop(&e->in_head, &e->in_tail);
        if (!old) break;
        e->in_len--;
        node_free(old);
    }
}

int katana_enc_submit(katana_enc *e, const uint8_t *nv12, int len) {
    if (!e || e->dead) return -1;
    uint8_t *copy = (uint8_t *)malloc(len);
    if (!copy) return -1;
    memcpy(copy, nv12, len);
    EnterCriticalSection(&e->lock);
    q_push(&e->in_head, &e->in_tail, copy, len);
    e->in_len++;
    drop_input_overflow_locked(e);
    enc_feed_locked(e);
    LeaveCriticalSection(&e->lock);
    return 0;
}

// ---- zero-copy: GPU-конверт BGRA→NV12 через ID3D11VideoProcessor ----

// vproc_init поднимает video processor под размеры src→dst (dst = e->width×height).
// Девайс с MT-protection сериализует контекст сам — LockDevice не нужен.
static HRESULT vproc_init(katana_enc *e, int src_w, int src_h) {
    if (!e->d3d || !e->d3dctx) return E_FAIL;
    HRESULT hr;

    hr = ID3D11Device_QueryInterface(e->d3d, &IID_ID3D11VideoDevice, (void **)&e->vdev);
    if (FAILED(hr)) return hr;
    hr = ID3D11DeviceContext_QueryInterface(e->d3dctx, &IID_ID3D11VideoContext, (void **)&e->vctx);
    if (FAILED(hr)) return hr;

    D3D11_VIDEO_PROCESSOR_CONTENT_DESC cd;
    memset(&cd, 0, sizeof(cd));
    cd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    cd.InputWidth = src_w;      cd.InputHeight = src_h;
    cd.OutputWidth = e->width;  cd.OutputHeight = e->height;
    cd.Usage = D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;
    hr = ID3D11VideoDevice_CreateVideoProcessorEnumerator(e->vdev, &cd, &e->vpenum);
    if (FAILED(hr)) return hr;
    hr = ID3D11VideoDevice_CreateVideoProcessor(e->vdev, e->vpenum, 0, &e->vp);
    if (FAILED(hr)) return hr;

    // Владеемая BGRA-текстура (вход VP) + её input view (создаётся один раз).
    D3D11_TEXTURE2D_DESC td;
    memset(&td, 0, sizeof(td));
    td.Width = src_w; td.Height = src_h; td.MipLevels = 1; td.ArraySize = 1;
    td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
    hr = ID3D11Device_CreateTexture2D(e->d3d, &td, NULL, &e->bgra_in);
    if (FAILED(hr)) return hr;

    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd;
    memset(&ivd, 0, sizeof(ivd));
    ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
    ivd.Texture2D.MipSlice = 0;
    hr = ID3D11VideoDevice_CreateVideoProcessorInputView(e->vdev, (ID3D11Resource *)e->bgra_in, e->vpenum, &ivd, &e->bgra_view);
    if (FAILED(hr)) return hr;

    // Пул NV12-текстур (выход VP + вход энкодера) + output views.
    for (int i = 0; i < KATANA_NV12_POOL; i++) {
        D3D11_TEXTURE2D_DESC nd;
        memset(&nd, 0, sizeof(nd));
        nd.Width = e->width; nd.Height = e->height; nd.MipLevels = 1; nd.ArraySize = 1;
        nd.Format = DXGI_FORMAT_NV12;
        nd.SampleDesc.Count = 1;
        nd.Usage = D3D11_USAGE_DEFAULT;
        nd.BindFlags = D3D11_BIND_RENDER_TARGET;
        hr = ID3D11Device_CreateTexture2D(e->d3d, &nd, NULL, &e->nv12tex[i]);
        if (FAILED(hr)) return hr;

        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd;
        memset(&ovd, 0, sizeof(ovd));
        ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
        ovd.Texture2D.MipSlice = 0;
        hr = ID3D11VideoDevice_CreateVideoProcessorOutputView(e->vdev, (ID3D11Resource *)e->nv12tex[i], e->vpenum, &ovd, &e->nv12view[i]);
        if (FAILED(hr)) return hr;
    }

    // Цветовое пространство: вход RGB full-range, выход YCbCr BT.601 studio (16-235) —
    // как у CPU-конвертера, чтобы не поехали цвета у зрителя.
    D3D11_VIDEO_PROCESSOR_COLOR_SPACE cs;
    memset(&cs, 0, sizeof(cs));
    cs.Usage = 0;        // playback
    cs.RGB_Range = 0;    // 0 = full
    cs.YCbCr_Matrix = 0; // 0 = BT.601
    cs.YCbCr_xvYCC = 0;
    cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
    ID3D11VideoContext_VideoProcessorSetOutputColorSpace(e->vctx, e->vp, &cs);
    ID3D11VideoContext_VideoProcessorSetStreamColorSpace(e->vctx, e->vp, 0, &cs);

    e->src_w = src_w; e->src_h = src_h;
    e->vp_ready = 1;
    return S_OK;
}

// katana_enc_init_vproc поднимает zero-copy конвейер под размер кадра захвата.
int katana_enc_init_vproc(katana_enc *e, int src_w, int src_h, int32_t *out_hr) {
    if (!e) return -1;
    EnterCriticalSection(&e->lock);
    HRESULT hr = vproc_init(e, src_w, src_h);
    LeaveCriticalSection(&e->lock);
    if (out_hr) *out_hr = hr;
    return SUCCEEDED(hr) ? 0 : -1;
}

// katana_enc_capture_texture копирует BGRA-кадр WGC в свою текстуру (пока tex жива).
// GPU-to-GPU, без CPU. Кодирование — отдельно (encode_captured), чтобы CFR-повтор при
// простое мог переиспользовать последний захваченный кадр без живого tex.
int katana_enc_capture_texture(katana_enc *e, void *bgra_tex) {
    if (!e || e->dead) return -1;
    if (!e->vp_ready) return -2;
    EnterCriticalSection(&e->lock);
    ID3D11DeviceContext_CopyResource(e->d3dctx, (ID3D11Resource *)e->bgra_in, (ID3D11Resource *)bgra_tex);
    LeaveCriticalSection(&e->lock);
    return 0;
}

// katana_enc_encode_captured конвертит последний захваченный кадр BGRA→NV12(+scale) на
// GPU, оборачивает NV12-текстуру в IMFSample и скармливает энкодеру. Без CPU-копий.
int katana_enc_encode_captured(katana_enc *e) {
    if (!e || e->dead) return -1;
    if (!e->vp_ready) return -2;
    EnterCriticalSection(&e->lock);

    int i = e->nv12_idx;
    e->nv12_idx = (e->nv12_idx + 1) % KATANA_NV12_POOL;

    D3D11_VIDEO_PROCESSOR_STREAM stream;
    memset(&stream, 0, sizeof(stream));
    stream.Enable = TRUE;
    stream.pInputSurface = e->bgra_view;
    HRESULT hr = ID3D11VideoContext_VideoProcessorBlt(e->vctx, e->vp, e->nv12view[i], 0, 1, &stream);
    if (FAILED(hr)) { LeaveCriticalSection(&e->lock); return -3; }

    // Оборачиваем NV12-текстуру в IMFSample (без копий).
    IMFMediaBuffer *buf = NULL;
    hr = MFCreateDXGISurfaceBuffer(&IID_ID3D11Texture2D, (IUnknown *)e->nv12tex[i], 0, FALSE, &buf);
    if (FAILED(hr) || !buf) { LeaveCriticalSection(&e->lock); return -4; }
    IMF2DBuffer *b2d = NULL;
    if (SUCCEEDED(IMFMediaBuffer_QueryInterface(buf, &IID_IMF2DBuffer, (void **)&b2d)) && b2d) {
        DWORD cbLen = 0;
        if (SUCCEEDED(IMF2DBuffer_GetContiguousLength(b2d, &cbLen)))
            IMFMediaBuffer_SetCurrentLength(buf, cbLen);
        IMF2DBuffer_Release(b2d);
    }
    IMFSample *sample = NULL;
    if (FAILED(MFCreateSample(&sample)) || !sample) {
        IMFMediaBuffer_Release(buf);
        LeaveCriticalSection(&e->lock);
        return -5;
    }
    IMFSample_AddBuffer(sample, buf);
    IMFMediaBuffer_Release(buf);

    q_push_sample(&e->in_head, &e->in_tail, sample);
    e->in_len++;
    drop_input_overflow_locked(e);
    enc_feed_locked(e);
    LeaveCriticalSection(&e->lock);
    return 0;
}

int katana_enc_poll(katana_enc *e, uint8_t *buf, int buflen) {
    if (!e) return -1;
    EnterCriticalSection(&e->lock);
    frame_node *n = q_pop(&e->out_head, &e->out_tail);
    if (n) e->out_len--;
    LeaveCriticalSection(&e->lock);
    if (!n) return 0;
    int len = n->len;
    if (len > buflen) { free(n->data); free(n); return -2; }
    memcpy(buf, n->data, len);
    free(n->data);
    free(n);
    return len;
}

// katana_enc_set_bitrate меняет целевой битрейт на лету (ответ на AIMD/потери сети).
// AMD AMF MFT принимает динамику через CODECAPI_AVEncCommonMeanBitRate — как Chromium.
void katana_enc_set_bitrate(katana_enc *e, int kbps) {
    if (!e || !e->codec || kbps <= 0) return;
    VARIANT v;
    VariantInit(&v);
    EnterCriticalSection(&e->lock);
    v.vt = VT_UI4; v.ulVal = (ULONG)kbps * 1000;
    ICodecAPI_SetValue(e->codec, &k_MeanBitRate, &v);
    ICodecAPI_SetValue(e->codec, &k_MaxBitRate, &v);
    v.vt = VT_UI4; v.ulVal = (ULONG)kbps * 1000 / 2; // VBV ~0.5с
    ICodecAPI_SetValue(e->codec, &k_BufferSize, &v);
    LeaveCriticalSection(&e->lock);
}

// katana_enc_force_keyframe просит энкодер выдать IDR на следующем кадре (ответ на PLI).
void katana_enc_force_keyframe(katana_enc *e) {
    if (!e || !e->codec) return;
    VARIANT v;
    VariantInit(&v);
    v.vt = VT_UI4; v.ulVal = 1;
    EnterCriticalSection(&e->lock);
    ICodecAPI_SetValue(e->codec, &k_ForceKeyFrame, &v);
    LeaveCriticalSection(&e->lock);
}

void katana_enc_destroy(katana_enc *e) {
    if (!e) return;
    InterlockedExchange(&e->dead, 1);
    if (e->mft) {
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_DRAIN, 0);
    }
    if (e->codec) ICodecAPI_Release(e->codec);
    if (e->evgen) IMFMediaEventGenerator_Release(e->evgen);
    if (e->mft) IMFTransform_Release(e->mft);
    if (e->devmgr) IMFDXGIDeviceManager_Release(e->devmgr);
    // zero-copy ресурсы.
    for (int i = 0; i < KATANA_NV12_POOL; i++) {
        if (e->nv12view[i]) ID3D11VideoProcessorOutputView_Release(e->nv12view[i]);
        if (e->nv12tex[i]) ID3D11Texture2D_Release(e->nv12tex[i]);
    }
    if (e->bgra_view) ID3D11VideoProcessorInputView_Release(e->bgra_view);
    if (e->bgra_in) ID3D11Texture2D_Release(e->bgra_in);
    if (e->vp) ID3D11VideoProcessor_Release(e->vp);
    if (e->vpenum) ID3D11VideoProcessorEnumerator_Release(e->vpenum);
    if (e->vctx) ID3D11VideoContext_Release(e->vctx);
    if (e->vdev) ID3D11VideoDevice_Release(e->vdev);
    if (e->d3dctx) ID3D11DeviceContext_Release(e->d3dctx);
    if (e->d3d) ID3D11Device_Release(e->d3d);
    // Чистим очереди.
    EnterCriticalSection(&e->lock);
    for (frame_node *n = e->in_head; n;) { frame_node *nx = n->next; node_free(n); n = nx; }
    for (frame_node *n = e->out_head; n;) { frame_node *nx = n->next; node_free(n); n = nx; }
    LeaveCriticalSection(&e->lock);
    DeleteCriticalSection(&e->lock);
    free(e);
}
