//go:build windows && cgo && winnative

// Нативный аппаратный H264-энкодер на AMD AMF SDK (в обход Media Foundation), делящий
// D3D11-устройство с WGC-захватом. См. amf_encoder_windows.h. Даёт слайсы + rolling
// intra-refresh, которые MF-обёртка AMD игнорит.
//
// Конвейер BGRA→NV12 (ID3D11VideoProcessor) — тот же, что в mf_encoder_windows.c; отличие
// лишь в финале: NV12-текстуру оборачиваем в AMFSurface (CreateSurfaceFromDX11Native,
// zero-copy) и шлём в AMFComponent::SubmitInput, а готовый Annex-B тянем QueryOutput.
// Модель poll-based (не async-callback как MF): submit на одной горутине, poll на другой.

#include <windows.h>
#include <d3d11.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include <stdint.h>

#include "core/Factory.h"
#include "core/Surface.h"
#include "core/Buffer.h"
#include "core/Plane.h"
#include "components/VideoEncoderVCE.h"

#include "amf_encoder_windows.h"

#define KATANA_AMF_POOL 4  // пул NV12-текстур (энкодер держит вход до обработки)

struct katana_amf {
    HMODULE dll;
    AMFFactory *factory;   // принадлежит рантайму, не релизим
    AMFContext *context;
    AMFComponent *enc;

    CRITICAL_SECTION lock; // сериализует вызовы AMF-компонента (submit/poll/setprop)
    int width, height, fps;
    LONGLONG frame_dur, sample_time; // 100-нс тики для pts
    volatile LONG dead;
    volatile LONG force_idr;
    long long got_slices, got_ir;    // readback фич для диагностики

    // --- zero-copy BGRA→NV12 через ID3D11VideoProcessor (как в MF) ---
    ID3D11Device *d3d;
    ID3D11DeviceContext *d3dctx;
    ID3D11VideoDevice *vdev;
    ID3D11VideoContext *vctx;
    ID3D11VideoProcessorEnumerator *vpenum;
    ID3D11VideoProcessor *vp;
    ID3D11Texture2D *bgra_in;
    ID3D11VideoProcessorInputView *bgra_view;
    ID3D11Texture2D *nv12tex[KATANA_AMF_POOL];
    ID3D11VideoProcessorOutputView *nv12view[KATANA_AMF_POOL];
    int nv12_idx;
    int src_w, src_h, vp_ready;
};

// ---- гейт: есть ли рантайм AMF ----
int katana_amf_available(void) {
    HMODULE h = LoadLibraryA("amfrt64.dll");
    if (!h) return 0;
    FreeLibrary(h);
    return 1;
}

// ---- конфигурация энкодера ----
static void amf_configure(katana_amf *e, int gop, int kbps) {
    AMFComponent *enc = e->enc;
    AMFVariantStruct v;
#define SET_I64(NAME, VAL) do { AMFVariantAssignInt64(&v, (amf_int64)(VAL)); \
    enc->pVtbl->SetProperty(enc, NAME, v); } while (0)

    // USAGE первым — он сбрасывает RC/прочее к пресету, дальше переопределяем.
    SET_I64(AMF_VIDEO_ENCODER_USAGE, AMF_VIDEO_ENCODER_USAGE_ULTRA_LOW_LATENCY);
    SET_I64(AMF_VIDEO_ENCODER_PROFILE, AMF_VIDEO_ENCODER_PROFILE_HIGH);       // как macOS
    SET_I64(AMF_VIDEO_ENCODER_QUALITY_PRESET, AMF_VIDEO_ENCODER_QUALITY_PRESET_BALANCED);
    SET_I64(AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD, AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD_CBR);
    SET_I64(AMF_VIDEO_ENCODER_TARGET_BITRATE, (amf_int64)kbps * 1000);
    SET_I64(AMF_VIDEO_ENCODER_PEAK_BITRATE, (amf_int64)kbps * 1000);
    { AMFSize sz = AMFConstructSize(e->width, e->height);
      AMFVariantAssignSize(&v, &sz); enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_FRAMESIZE, v); }
    { AMFRate fr = AMFConstructRate(e->fps, 1);
      AMFVariantAssignRate(&v, &fr); enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_FRAMERATE, v); }
    SET_I64(AMF_VIDEO_ENCODER_IDR_PERIOD, gop > 0 ? gop : e->fps);
    SET_I64(AMF_VIDEO_ENCODER_B_PIC_PATTERN, 0);                              // без B-кадров (латенси)

    // >>> ради чего всё: локализация потерь, которую MF не давал <<<
    SET_I64(AMF_VIDEO_ENCODER_SLICES_PER_FRAME, 4);                           // потеря = четверть кадра
    int mbW = (e->width + 15) / 16, mbH = (e->height + 15) / 16, tot = mbW * mbH;
    int ir = (tot + e->fps - 1) / e->fps;                                     // полный рефреш ~ за 1с
    SET_I64(AMF_VIDEO_ENCODER_INTRA_REFRESH_NUM_MBS_PER_SLOT, ir);
#undef SET_I64
}

katana_amf *katana_amf_create(void *d3d_device, int width, int height, int fps,
                              int bitrate_kbps, int gop, int32_t *out_hr, int *out_stage,
                              char *out_info, int info_cap) {
    int stage = 0;
    AMF_RESULT res = AMF_OK;
    if (out_info && info_cap > 0) out_info[0] = 0;

    katana_amf *e = (katana_amf *)calloc(1, sizeof(katana_amf));
    if (!e) { if (out_hr) *out_hr = (int32_t)E_OUTOFMEMORY; if (out_stage) *out_stage = 0; return NULL; }
    e->width = width; e->height = height; e->fps = fps > 0 ? fps : 30;
    e->frame_dur = 10000000LL / e->fps;
    InitializeCriticalSection(&e->lock);

    stage = 1;
    e->dll = LoadLibraryA("amfrt64.dll");
    if (!e->dll) { res = (AMF_RESULT)AMF_FAIL; goto fail; }

    AMFInit_Fn init = (AMFInit_Fn)(void *)GetProcAddress(e->dll, AMF_INIT_FUNCTION_NAME);
    if (!init) { res = (AMF_RESULT)AMF_FAIL; goto fail; }

    stage = 2;
    res = init(AMF_FULL_VERSION, &e->factory);
    if (res != AMF_OK || !e->factory) goto fail;

    stage = 3;
    res = e->factory->pVtbl->CreateContext(e->factory, &e->context);
    if (res != AMF_OK || !e->context) goto fail;

    stage = 4;
    // Делим D3D11-устройство с захватом (тот же девайс — zero-copy из WGC-конвейера).
    res = e->context->pVtbl->InitDX11(e->context, d3d_device, AMF_DX11_1);
    if (res != AMF_OK) res = e->context->pVtbl->InitDX11(e->context, d3d_device, AMF_DX11_0);
    if (res != AMF_OK) goto fail;
    // Держим сам девайс и immediate context для VP-конверта (как MF).
    if (d3d_device) {
        e->d3d = (ID3D11Device *)d3d_device;
        ID3D11Device_AddRef(e->d3d);
        ID3D11Device_GetImmediateContext(e->d3d, &e->d3dctx);
    }

    stage = 5;
    res = e->factory->pVtbl->CreateComponent(e->factory, e->context, AMFVideoEncoderVCE_AVC, &e->enc);
    if (res != AMF_OK || !e->enc) goto fail;

    stage = 6;
    amf_configure(e, gop, bitrate_kbps);
    res = e->enc->pVtbl->Init(e->enc, AMF_SURFACE_NV12, width, height);
    if (res != AMF_OK) goto fail;

    // Readback: приняло ли железо слайсы/intra-refresh (в MF было IGNORED).
    AMFVariantStruct v;
    e->got_slices = e->got_ir = -1;
    if (e->enc->pVtbl->GetProperty(e->enc, AMF_VIDEO_ENCODER_SLICES_PER_FRAME, &v) == AMF_OK)
        e->got_slices = (long long)v.int64Value;
    if (e->enc->pVtbl->GetProperty(e->enc, AMF_VIDEO_ENCODER_INTRA_REFRESH_NUM_MBS_PER_SLOT, &v) == AMF_OK)
        e->got_ir = (long long)v.int64Value;
    if (out_info && info_cap > 0)
        snprintf(out_info, (size_t)info_cap, "AMF AVC High %dx%d@%d %dkbps slices=%lld ir=%lld",
                 width, height, e->fps, bitrate_kbps, e->got_slices, e->got_ir);

    if (out_hr) *out_hr = 0;
    if (out_stage) *out_stage = 0;
    return e;

fail:
    if (out_hr) *out_hr = (int32_t)res;
    if (out_stage) *out_stage = stage;
    katana_amf_destroy(e);
    return NULL;
}

int katana_amf_extradata(katana_amf *e, uint8_t *buf, int cap) {
    if (!e || !e->enc || !buf || cap <= 0) return 0;
    AMFVariantStruct v;
    int n = 0;
    EnterCriticalSection(&e->lock);
    if (e->enc->pVtbl->GetProperty(e->enc, AMF_VIDEO_ENCODER_EXTRADATA, &v) == AMF_OK
        && v.type == AMF_VARIANT_INTERFACE && v.pInterface) {
        AMFBuffer *xb = (AMFBuffer *)v.pInterface;
        int xs = (int)xb->pVtbl->GetSize(xb);
        const uint8_t *xd = (const uint8_t *)xb->pVtbl->GetNative(xb);
        if (xd && xs > 0 && xs <= cap) { memcpy(buf, xd, (size_t)xs); n = xs; }
        xb->pVtbl->Release(xb);
    }
    LeaveCriticalSection(&e->lock);
    return n;
}

// ---- submit AMFSurface в энкодер (под e->lock). Дропает кадр при INPUT_FULL. ----
static int amf_submit_surface_locked(katana_amf *e, AMFSurface *surf) {
    AMFData *data = (AMFData *)surf;
    data->pVtbl->SetPts(data, e->sample_time);
    e->sample_time += e->frame_dur;
    if (InterlockedExchange(&e->force_idr, 0)) {
        AMFVariantStruct v;
        AMFVariantAssignInt64(&v, AMF_VIDEO_ENCODER_PICTURE_TYPE_IDR);
        surf->pVtbl->SetProperty(surf, AMF_VIDEO_ENCODER_FORCE_PICTURE_TYPE, v);
        AMFVariantAssignInt64(&v, 1); surf->pVtbl->SetProperty(surf, AMF_VIDEO_ENCODER_INSERT_SPS, v);
        AMFVariantAssignInt64(&v, 1); surf->pVtbl->SetProperty(surf, AMF_VIDEO_ENCODER_INSERT_PPS, v);
    }
    AMF_RESULT sr = e->enc->pVtbl->SubmitInput(e->enc, data);
    surf->pVtbl->Release(surf);      // AMF держит свою ссылку
    if (sr == AMF_INPUT_FULL) return 0;   // энкодер отстаёт — дропаем кадр (realtime-экран)
    return sr == AMF_OK ? 0 : -5;
}

// ---- байтовый путь: NV12 из системной памяти → HOST-surface → энкодер ----
int katana_amf_submit(katana_amf *e, const uint8_t *nv12, int len) {
    if (!e || e->dead) return -1;
    int need = e->width * e->height * 3 / 2;
    if (len < need) return -1;
    EnterCriticalSection(&e->lock);
    AMFSurface *surf = NULL;
    AMF_RESULT r = e->context->pVtbl->AllocSurface(e->context, AMF_MEMORY_HOST,
                                                   AMF_SURFACE_NV12, e->width, e->height, &surf);
    if (r != AMF_OK || !surf) { LeaveCriticalSection(&e->lock); return -4; }
    // копируем Y и UV по плоскостям (с учётом pitch).
    AMFPlane *py = surf->pVtbl->GetPlane(surf, AMF_PLANE_Y);
    AMFPlane *puv = surf->pVtbl->GetPlane(surf, AMF_PLANE_UV);
    if (py) {
        uint8_t *dst = (uint8_t *)py->pVtbl->GetNative(py);
        int pitch = py->pVtbl->GetHPitch(py);
        for (int y = 0; y < e->height; y++)
            memcpy(dst + (size_t)y * pitch, nv12 + (size_t)y * e->width, e->width);
    }
    if (puv) {
        uint8_t *dst = (uint8_t *)puv->pVtbl->GetNative(puv);
        int pitch = puv->pVtbl->GetHPitch(puv);
        const uint8_t *src = nv12 + (size_t)e->width * e->height;
        for (int y = 0; y < e->height / 2; y++)
            memcpy(dst + (size_t)y * pitch, src + (size_t)y * e->width, e->width);
    }
    int rc = amf_submit_surface_locked(e, surf);
    LeaveCriticalSection(&e->lock);
    return rc;
}

int katana_amf_poll(katana_amf *e, uint8_t *buf, int buflen) {
    if (!e || e->dead || !e->enc) return -1;
    EnterCriticalSection(&e->lock);
    AMFData *out = NULL;
    AMF_RESULT r = e->enc->pVtbl->QueryOutput(e->enc, &out);
    LeaveCriticalSection(&e->lock);
    if (r != AMF_OK || !out) return 0;
    AMFBuffer *ob = (AMFBuffer *)out;
    int n = (int)ob->pVtbl->GetSize(ob);
    const uint8_t *p = (const uint8_t *)ob->pVtbl->GetNative(ob);
    if (n > buflen) { out->pVtbl->Release(out); return -2; }
    if (p && n > 0) memcpy(buf, p, (size_t)n);
    out->pVtbl->Release(out);
    return n;
}

// ---- zero-copy: VideoProcessor BGRA→NV12 (структура как в MF) ----
static HRESULT vproc_init(katana_amf *e, int src_w, int src_h) {
    if (!e->d3d || !e->d3dctx) return E_FAIL;
    HRESULT hr;
    hr = ID3D11Device_QueryInterface(e->d3d, &IID_ID3D11VideoDevice, (void **)&e->vdev);
    if (FAILED(hr)) return hr;
    hr = ID3D11DeviceContext_QueryInterface(e->d3dctx, &IID_ID3D11VideoContext, (void **)&e->vctx);
    if (FAILED(hr)) return hr;

    D3D11_VIDEO_PROCESSOR_CONTENT_DESC cd;
    memset(&cd, 0, sizeof(cd));
    cd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    cd.InputWidth = src_w;     cd.InputHeight = src_h;
    cd.OutputWidth = e->width; cd.OutputHeight = e->height;
    cd.Usage = D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;
    hr = ID3D11VideoDevice_CreateVideoProcessorEnumerator(e->vdev, &cd, &e->vpenum);
    if (FAILED(hr)) return hr;
    hr = ID3D11VideoDevice_CreateVideoProcessor(e->vdev, e->vpenum, 0, &e->vp);
    if (FAILED(hr)) return hr;

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

    for (int i = 0; i < KATANA_AMF_POOL; i++) {
        D3D11_TEXTURE2D_DESC nd;
        memset(&nd, 0, sizeof(nd));
        nd.Width = e->width; nd.Height = e->height; nd.MipLevels = 1; nd.ArraySize = 1;
        nd.Format = DXGI_FORMAT_NV12;
        nd.SampleDesc.Count = 1;
        nd.Usage = D3D11_USAGE_DEFAULT;
        nd.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
        hr = ID3D11Device_CreateTexture2D(e->d3d, &nd, NULL, &e->nv12tex[i]);
        if (FAILED(hr)) return hr;

        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd;
        memset(&ovd, 0, sizeof(ovd));
        ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
        ovd.Texture2D.MipSlice = 0;
        hr = ID3D11VideoDevice_CreateVideoProcessorOutputView(e->vdev, (ID3D11Resource *)e->nv12tex[i], e->vpenum, &ovd, &e->nv12view[i]);
        if (FAILED(hr)) return hr;
    }

    D3D11_VIDEO_PROCESSOR_COLOR_SPACE cs;
    memset(&cs, 0, sizeof(cs));
    cs.RGB_Range = 0;    // full
    cs.YCbCr_Matrix = 0; // BT.601
    cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
    ID3D11VideoContext_VideoProcessorSetOutputColorSpace(e->vctx, e->vp, &cs);
    ID3D11VideoContext_VideoProcessorSetStreamColorSpace(e->vctx, e->vp, 0, &cs);

    e->src_w = src_w; e->src_h = src_h;
    e->vp_ready = 1;
    return S_OK;
}

int katana_amf_init_vproc(katana_amf *e, int src_w, int src_h, int32_t *out_hr) {
    if (!e) return -1;
    EnterCriticalSection(&e->lock);
    HRESULT hr = vproc_init(e, src_w, src_h);
    LeaveCriticalSection(&e->lock);
    if (out_hr) *out_hr = (int32_t)hr;
    return SUCCEEDED(hr) ? 0 : -1;
}

int katana_amf_capture_texture(katana_amf *e, void *bgra_tex) {
    if (!e || e->dead) return -1;
    if (!e->vp_ready) return -2;
    EnterCriticalSection(&e->lock);
    ID3D11DeviceContext_CopyResource(e->d3dctx, (ID3D11Resource *)e->bgra_in, (ID3D11Resource *)bgra_tex);
    LeaveCriticalSection(&e->lock);
    return 0;
}

int katana_amf_encode_captured(katana_amf *e) {
    if (!e || e->dead) return -1;
    if (!e->vp_ready) return -2;
    EnterCriticalSection(&e->lock);
    int i = e->nv12_idx;
    e->nv12_idx = (e->nv12_idx + 1) % KATANA_AMF_POOL;

    D3D11_VIDEO_PROCESSOR_STREAM stream;
    memset(&stream, 0, sizeof(stream));
    stream.Enable = TRUE;
    stream.pInputSurface = e->bgra_view;
    HRESULT hr = ID3D11VideoContext_VideoProcessorBlt(e->vctx, e->vp, e->nv12view[i], 0, 1, &stream);
    if (FAILED(hr)) { LeaveCriticalSection(&e->lock); return -3; }

    AMFSurface *surf = NULL;
    AMF_RESULT r = e->context->pVtbl->CreateSurfaceFromDX11Native(e->context, (void *)e->nv12tex[i], &surf, NULL);
    if (r != AMF_OK || !surf) { LeaveCriticalSection(&e->lock); return -4; }
    int rc = amf_submit_surface_locked(e, surf);
    LeaveCriticalSection(&e->lock);
    return rc;
}

void katana_amf_force_keyframe(katana_amf *e) {
    if (!e) return;
    InterlockedExchange(&e->force_idr, 1);
}

void katana_amf_set_bitrate(katana_amf *e, int kbps) {
    if (!e || !e->enc || kbps <= 0) return;
    AMFVariantStruct v;
    EnterCriticalSection(&e->lock);
    AMFVariantAssignInt64(&v, (amf_int64)kbps * 1000);
    e->enc->pVtbl->SetProperty(e->enc, AMF_VIDEO_ENCODER_TARGET_BITRATE, v);
    AMFVariantAssignInt64(&v, (amf_int64)kbps * 1000);
    e->enc->pVtbl->SetProperty(e->enc, AMF_VIDEO_ENCODER_PEAK_BITRATE, v);
    LeaveCriticalSection(&e->lock);
}

void katana_amf_destroy(katana_amf *e) {
    if (!e) return;
    InterlockedExchange(&e->dead, 1);
    if (e->enc) {
        e->enc->pVtbl->Drain(e->enc);
        e->enc->pVtbl->Terminate(e->enc);
        e->enc->pVtbl->Release(e->enc);
    }
    // zero-copy ресурсы.
    for (int i = 0; i < KATANA_AMF_POOL; i++) {
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
    if (e->context) {
        e->context->pVtbl->Terminate(e->context);
        e->context->pVtbl->Release(e->context);
    }
    // factory принадлежит рантайму — не релизим.
    if (e->dll) FreeLibrary(e->dll);
    DeleteCriticalSection(&e->lock);
    free(e);
}
