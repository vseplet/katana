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
#include <string.h>
#include <stdlib.h>

#include "mf_encoder_windows.h"

// Типы событий асинхронного MFT (на случай, если тулчейн их не объявил).
#ifndef METransformNeedInput
#define METransformNeedInput 601
#endif
#ifndef METransformHaveOutput
#define METransformHaveOutput 602
#endif

// --- очередь кадров (односвязный список копий буферов) ---
typedef struct frame_node {
    struct frame_node *next;
    uint8_t *data;
    int len;
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
    async_cb cb;

    CRITICAL_SECTION lock;
    frame_node *in_head, *in_tail;
    frame_node *out_head, *out_tail;
    int need_input; // сколько NeedInput пришло, пока очередь входа была пуста

    LONGLONG sample_time; // 100-нс тики
    LONGLONG frame_dur;
    int width, height, fps;
    UINT reset_token;
    volatile LONG dead;
};

// ---- очередь: push/pop под внешним локом ----
static void q_push(frame_node **head, frame_node **tail, uint8_t *data, int len) {
    frame_node *n = (frame_node *)malloc(sizeof(frame_node));
    if (!n) { free(data); return; }
    n->next = NULL; n->data = data; n->len = len;
    if (*tail) (*tail)->next = n; else *head = n;
    *tail = n;
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

// ---- скармливание входа: под e->lock, пока есть и запросы, и кадры ----
static void enc_feed_locked(katana_enc *e) {
    while (e->need_input > 0 && e->in_head) {
        frame_node *n = q_pop(&e->in_head, &e->in_tail);
        if (!n) break;

        IMFSample *sample = NULL;
        IMFMediaBuffer *buf = NULL;
        if (SUCCEEDED(MFCreateSample(&sample)) &&
            SUCCEEDED(MFCreateMemoryBuffer(n->len, &buf))) {
            BYTE *dst = NULL; DWORD maxlen = 0;
            if (SUCCEEDED(IMFMediaBuffer_Lock(buf, &dst, &maxlen, NULL))) {
                memcpy(dst, n->data, n->len);
                IMFMediaBuffer_Unlock(buf);
                IMFMediaBuffer_SetCurrentLength(buf, n->len);
                IMFSample_AddBuffer(sample, buf);
                IMFSample_SetSampleTime(sample, e->sample_time);
                IMFSample_SetSampleDuration(sample, e->frame_dur);
                e->sample_time += e->frame_dur;
                IMFTransform_ProcessInput(e->mft, 0, sample, 0);
                e->need_input--;
            }
        }
        if (buf) IMFMediaBuffer_Release(buf);
        if (sample) IMFSample_Release(sample);
        free(n->data);
        free(n);
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
    IMFMediaType_SetUINT32(t, &MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Base);
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

// TODO: CBR + размер GOP + low-latency через ICodecAPI. В mingw заголовки ICodecAPI
// капризничают (нужен отдельный заход), пока полагаемся на дефолтный rate-control
// энкодера и битрейт из MF_MT_AVG_BITRATE. gop прокидываем, но пока не применяем.
static void configure_codecapi(katana_enc *e, int gop) {
    (void)e; (void)gop;
}

katana_enc *katana_enc_create(void *d3d_device, int width, int height, int fps,
                              int bitrate_kbps, int gop, int32_t *out_hr) {
    HRESULT hr = S_OK;
    katana_enc *e = (katana_enc *)calloc(1, sizeof(katana_enc));
    if (!e) { if (out_hr) *out_hr = E_OUTOFMEMORY; return NULL; }
    e->width = width; e->height = height; e->fps = fps > 0 ? fps : 30;
    e->frame_dur = 10000000LL / e->fps;
    e->cb.lpVtbl = &g_cb_vtbl;
    e->cb.ref = 1;
    e->cb.enc = e;
    InitializeCriticalSection(&e->lock);

    hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
    if (FAILED(hr)) goto fail;

    // Ищем аппаратный H264-энкодер.
    MFT_REGISTER_TYPE_INFO out_info = { MFMediaType_Video, MFVideoFormat_H264 };
    IMFActivate **acts = NULL;
    UINT32 count = 0;
    hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                   MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                   NULL, &out_info, &acts, &count);
    if (FAILED(hr) || count == 0) {
        if (SUCCEEDED(hr)) hr = MF_E_NOT_FOUND;
        goto fail;
    }
    hr = IMFActivate_ActivateObject(acts[0], &IID_IMFTransform, (void **)&e->mft);
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
    hr = set_output_type(e, bitrate_kbps);
    if (FAILED(hr)) goto fail;
    hr = set_input_type(e);
    if (FAILED(hr)) goto fail;
    configure_codecapi(e, gop);

    hr = IMFTransform_QueryInterface(e->mft, &IID_IMFMediaEventGenerator, (void **)&e->evgen);
    if (FAILED(hr)) goto fail;

    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);

    // Запускаем цикл событий.
    hr = IMFMediaEventGenerator_BeginGetEvent(e->evgen, (IMFAsyncCallback *)&e->cb, NULL);
    if (FAILED(hr)) goto fail;

    if (out_hr) *out_hr = S_OK;
    return e;

fail:
    if (out_hr) *out_hr = hr;
    katana_enc_destroy(e);
    return NULL;
}

int katana_enc_submit(katana_enc *e, const uint8_t *nv12, int len) {
    if (!e || e->dead) return -1;
    uint8_t *copy = (uint8_t *)malloc(len);
    if (!copy) return -1;
    memcpy(copy, nv12, len);
    EnterCriticalSection(&e->lock);
    q_push(&e->in_head, &e->in_tail, copy, len);
    enc_feed_locked(e);
    LeaveCriticalSection(&e->lock);
    return 0;
}

int katana_enc_poll(katana_enc *e, uint8_t *buf, int buflen) {
    if (!e) return -1;
    EnterCriticalSection(&e->lock);
    frame_node *n = q_pop(&e->out_head, &e->out_tail);
    LeaveCriticalSection(&e->lock);
    if (!n) return 0;
    int len = n->len;
    if (len > buflen) { free(n->data); free(n); return -2; }
    memcpy(buf, n->data, len);
    free(n->data);
    free(n);
    return len;
}

void katana_enc_destroy(katana_enc *e) {
    if (!e) return;
    InterlockedExchange(&e->dead, 1);
    if (e->mft) {
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        IMFTransform_ProcessMessage(e->mft, MFT_MESSAGE_COMMAND_DRAIN, 0);
    }
    if (e->evgen) IMFMediaEventGenerator_Release(e->evgen);
    if (e->mft) IMFTransform_Release(e->mft);
    if (e->devmgr) IMFDXGIDeviceManager_Release(e->devmgr);
    // Чистим очереди.
    EnterCriticalSection(&e->lock);
    for (frame_node *n = e->in_head; n;) { frame_node *nx = n->next; free(n->data); free(n); n = nx; }
    for (frame_node *n = e->out_head; n;) { frame_node *nx = n->next; free(n->data); free(n); n = nx; }
    LeaveCriticalSection(&e->lock);
    DeleteCriticalSection(&e->lock);
    free(e);
}
