//go:build windows && cgo && winnative

// Автономный пробник AMD AMF SDK. Через официальный C-API AMF (vtable-структуры,
// как COBJMACROS у Media Foundation) поднимает аппаратный H264-энкодер, просит High
// профиль + слайсы + rolling intra-refresh и проверяет readback'ом, что железо это
// РЕАЛЬНО приняло (в MF оба игнорились). Затем гоняет 60 серых NV12-кадров и считает
// число VCL-NAL (слайсов) в выходном битстриме. Ничего общего с основным стримом.

#include <windows.h>
#include <stdio.h>
#include <stdarg.h>
#include <string.h>
#include <stdint.h>

#include "core/Factory.h"
#include "core/Surface.h"
#include "core/Plane.h"
#include "core/Buffer.h"
#include "components/VideoEncoderVCE.h"

#include "amf_probe.h"

// ---- аккумулятор отчёта ----
typedef struct { char *b; int cap; int len; } rep_t;

static void RP(rep_t *r, const char *fmt, ...) {
    if (!r->b || r->len >= r->cap) return;
    va_list ap;
    va_start(ap, fmt);
    int n = vsnprintf(r->b + r->len, (size_t)(r->cap - r->len), fmt, ap);
    va_end(ap);
    if (n > 0) r->len += n;
    if (r->len > r->cap) r->len = r->cap;
}

// ---- разбор Annex-B: считаем VCL-NAL (слайсы) и метим IDR / SEI ----
static void count_nals(const uint8_t *b, int n, int *vcl, int *idr, int *sei) {
    int i = 0;
    while (i + 3 <= n) {
        int sc = 0;
        if (b[i] == 0 && b[i+1] == 0 && b[i+2] == 1) sc = 3;
        else if (i + 4 <= n && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1) sc = 4;
        if (!sc) { i++; continue; }
        int t = b[i + sc] & 0x1f;
        if (t == 1 || t == 5) (*vcl)++;   // VCL-слайс (не-IDR / IDR)
        if (t == 5) *idr = 1;
        if (t == 6) *sei = 1;
        i += sc + 1;
    }
}

// profile_idc из SPS (nal type 7): байт сразу после nal-заголовка.
static int find_profile(const uint8_t *b, int n) {
    int i = 0;
    while (i + 3 <= n) {
        int sc = 0;
        if (b[i] == 0 && b[i+1] == 0 && b[i+2] == 1) sc = 3;
        else if (i + 4 <= n && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1) sc = 4;
        if (!sc) { i++; continue; }
        int t = b[i + sc] & 0x1f;
        if (t == 7 && i + sc + 1 < n) return b[i + sc + 1];
        i += sc + 1;
    }
    return -1;
}

// заливаем серым (Y=128, UV=128) — контент не важен, важно что энкодер что-то жуёт.
static void fill_gray(AMFSurface *s) {
    AMFPlane *y  = s->pVtbl->GetPlane(s, AMF_PLANE_Y);
    AMFPlane *uv = s->pVtbl->GetPlane(s, AMF_PLANE_UV);
    if (y) {
        uint8_t *p = (uint8_t *)y->pVtbl->GetNative(y);
        int pitch = y->pVtbl->GetHPitch(y), h = y->pVtbl->GetHeight(y);
        if (p && pitch > 0 && h > 0) memset(p, 128, (size_t)pitch * h);
    }
    if (uv) {
        uint8_t *p = (uint8_t *)uv->pVtbl->GetNative(uv);
        int pitch = uv->pVtbl->GetHPitch(uv), h = uv->pVtbl->GetHeight(uv);
        if (p && pitch > 0 && h > 0) memset(p, 128, (size_t)pitch * h);
    }
}

// тянем и разбираем один готовый выход (если есть). Возвращает 1 если что-то забрали.
static int pull_output(AMFComponent *enc, int *got, int *totalVCL, int *firstSize,
                       int *sawIDR, int *sawSEI) {
    AMFData *out = NULL;
    AMF_RESULT r = enc->pVtbl->QueryOutput(enc, &out);
    if (r != AMF_OK || !out) return 0;
    AMFBuffer *ob = (AMFBuffer *)out;            // выход AVC-энкодера — AMFBuffer (: AMFData)
    int os = (int)ob->pVtbl->GetSize(ob);
    const uint8_t *od = (const uint8_t *)ob->pVtbl->GetNative(ob);
    int vcl = 0, idr = 0, sei = 0;
    if (od && os > 0) count_nals(od, os, &vcl, &idr, &sei);
    *totalVCL += vcl;
    if (idr) *sawIDR = 1;
    if (sei) *sawSEI = 1;
    if (*firstSize == 0) *firstSize = os;
    (*got)++;
    out->pVtbl->Release(out);
    return 1;
}

int amf_probe(char *out, int cap) {
    rep_t R = { out, cap, 0 };
    RP(&R, "== AMF probe (headers 1.5.2) ==\n");

    HMODULE dll = LoadLibraryA("amfrt64.dll");
    if (!dll) {
        RP(&R, "FAIL: LoadLibrary amfrt64.dll (err=%lu). Драйвер AMD без AMF / не AMD GPU.\n",
           (unsigned long)GetLastError());
        return R.len;
    }
    RP(&R, "ok  amfrt64.dll загружен\n");

    AMFInit_Fn init = (AMFInit_Fn)(void *)GetProcAddress(dll, AMF_INIT_FUNCTION_NAME);
    AMFQueryVersion_Fn qv = (AMFQueryVersion_Fn)(void *)GetProcAddress(dll, "AMFQueryVersion");
    if (!init) { RP(&R, "FAIL: нет экспорта AMFInit\n"); return R.len; }
    if (qv) {
        amf_uint64 rt = 0;
        if (qv(&rt) == AMF_OK)
            RP(&R, "ok  версия рантайма %llu.%llu.%llu.%llu\n",
               (unsigned long long)((rt >> 48) & 0xffff), (unsigned long long)((rt >> 32) & 0xffff),
               (unsigned long long)((rt >> 16) & 0xffff), (unsigned long long)(rt & 0xffff));
    }

    AMFFactory *factory = NULL;
    AMF_RESULT res = init(AMF_FULL_VERSION, &factory);
    if (res != AMF_OK || !factory) { RP(&R, "FAIL: AMFInit res=%d\n", (int)res); return R.len; }
    RP(&R, "ok  AMFInit -> factory (ABI mingw<->amfrt64 жив)\n");

    AMFContext *ctx = NULL;
    res = factory->pVtbl->CreateContext(factory, &ctx);
    if (res != AMF_OK || !ctx) { RP(&R, "FAIL: CreateContext res=%d\n", (int)res); return R.len; }
    RP(&R, "ok  CreateContext\n");

    res = ctx->pVtbl->InitDX11(ctx, NULL, AMF_DX11_1);       // NULL => AMF создаёт свой DX11-девайс
    if (res != AMF_OK) res = ctx->pVtbl->InitDX11(ctx, NULL, AMF_DX11_0);
    if (res != AMF_OK) {
        RP(&R, "FAIL: InitDX11 res=%d\n", (int)res);
        ctx->pVtbl->Terminate(ctx); ctx->pVtbl->Release(ctx);
        return R.len;
    }
    RP(&R, "ok  InitDX11 (внутренний девайс)\n");

    AMFComponent *enc = NULL;
    res = factory->pVtbl->CreateComponent(factory, ctx, AMFVideoEncoderVCE_AVC, &enc);
    if (res != AMF_OK || !enc) {
        RP(&R, "FAIL: CreateComponent AVC res=%d (нет аппаратного AVC-энкодера?)\n", (int)res);
        ctx->pVtbl->Terminate(ctx); ctx->pVtbl->Release(ctx);
        return R.len;
    }
    RP(&R, "ok  CreateComponent(AMFVideoEncoderVCE_AVC)\n");

    const int W = 1280, H = 720, FPS = 60, KBPS = 4000, SLICES = 4;
    int mbW = (W + 15) / 16, mbH = (H + 15) / 16, totalMB = mbW * mbH;
    int irPerSlot = (totalMB + FPS - 1) / FPS;   // полный проход рефреша ~ за FPS кадров (1с)

    AMFVariantStruct v;
    // USAGE ставим первым — он сбрасывает RC/прочее к пресету, дальше переопределяем.
    AMFVariantAssignInt64(&v, AMF_VIDEO_ENCODER_USAGE_ULTRA_LOW_LATENCY);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_USAGE, v);
    AMFVariantAssignInt64(&v, AMF_VIDEO_ENCODER_PROFILE_HIGH);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_PROFILE, v);
    AMFVariantAssignInt64(&v, AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD_CBR);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD, v);
    AMFVariantAssignInt64(&v, (amf_int64)KBPS * 1000);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_TARGET_BITRATE, v);
    { AMFSize sz = AMFConstructSize(W, H); AMFVariantAssignSize(&v, &sz);
      enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_FRAMESIZE, v); }
    { AMFRate fr = AMFConstructRate(FPS, 1); AMFVariantAssignRate(&v, &fr);
      enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_FRAMERATE, v); }
    AMFVariantAssignInt64(&v, FPS);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_IDR_PERIOD, v);
    AMFVariantAssignInt64(&v, 0);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_B_PIC_PATTERN, v);
    // >>> ради чего всё: слайсы + intra-refresh, которые Media Foundation игнорил <<<
    AMFVariantAssignInt64(&v, SLICES);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_SLICES_PER_FRAME, v);
    AMFVariantAssignInt64(&v, irPerSlot);
    enc->pVtbl->SetProperty(enc, AMF_VIDEO_ENCODER_INTRA_REFRESH_NUM_MBS_PER_SLOT, v);

    res = enc->pVtbl->Init(enc, AMF_SURFACE_NV12, W, H);
    if (res != AMF_OK) {
        RP(&R, "FAIL: enc Init res=%d\n", (int)res);
        enc->pVtbl->Release(enc);
        ctx->pVtbl->Terminate(ctx); ctx->pVtbl->Release(ctx);
        return R.len;
    }
    RP(&R, "ok  enc Init %dx%d@%d %dkbps High CBR\n", W, H, FPS, KBPS);

    // --- readback: приняло ли ЖЕЛЕЗО то, что MF молча игнорил ---
    long long gotSlices = -1, gotIR = -1, gotProfile = -1;
    if (enc->pVtbl->GetProperty(enc, AMF_VIDEO_ENCODER_SLICES_PER_FRAME, &v) == AMF_OK)
        gotSlices = (long long)v.int64Value;
    if (enc->pVtbl->GetProperty(enc, AMF_VIDEO_ENCODER_INTRA_REFRESH_NUM_MBS_PER_SLOT, &v) == AMF_OK)
        gotIR = (long long)v.int64Value;
    if (enc->pVtbl->GetProperty(enc, AMF_VIDEO_ENCODER_PROFILE, &v) == AMF_OK)
        gotProfile = (long long)v.int64Value;
    RP(&R, ">>> readback: profile=%lld  slices/frame=%lld (просили %d)  intraRefreshMBs/slot=%lld (просили %d)\n",
       gotProfile, gotSlices, SLICES, gotIR, irPerSlot);
    RP(&R, ">>> СЛАЙСЫ: %s | INTRA-REFRESH: %s   (в Media Foundation оба были IGNORED)\n",
       gotSlices == SLICES ? "ПРИНЯТЫ" : "НЕ приняты",
       gotIR == irPerSlot ? "ПРИНЯТ" : "НЕ принят");

    // SPS/PPS из ExtraData — реальный profile_idc.
    if (enc->pVtbl->GetProperty(enc, AMF_VIDEO_ENCODER_EXTRADATA, &v) == AMF_OK
        && v.type == AMF_VARIANT_INTERFACE && v.pInterface) {
        AMFBuffer *xb = (AMFBuffer *)v.pInterface;
        int xs = (int)xb->pVtbl->GetSize(xb);
        const uint8_t *xd = (const uint8_t *)xb->pVtbl->GetNative(xb);
        RP(&R, "ok  ExtraData(SPS/PPS) %d байт, SPS profile_idc=%d (100=High)\n",
           xs, find_profile(xd, xs));
        xb->pVtbl->Release(xb);
    }

    // --- прогон 60 серых кадров, считаем реальные слайсы в выходе ---
    AMFSurface *surf = NULL;
    res = ctx->pVtbl->AllocSurface(ctx, AMF_MEMORY_HOST, AMF_SURFACE_NV12, W, H, &surf);
    if (res != AMF_OK || !surf) {
        RP(&R, "FAIL: AllocSurface res=%d\n", (int)res);
        enc->pVtbl->Terminate(enc); enc->pVtbl->Release(enc);
        ctx->pVtbl->Terminate(ctx); ctx->pVtbl->Release(ctx);
        return R.len;
    }
    fill_gray(surf);

    int submitted = 0, got = 0, totalVCL = 0, firstSize = 0, sawIDR = 0, sawSEI = 0;
    AMFData *in = (AMFData *)surf;               // AMFSurface : AMFData
    for (int f = 0; f < 60; f++) {
        in->pVtbl->SetPts(in, (amf_pts)f * (10000000 / FPS));
        AMF_RESULT sr;
        int tries = 0;
        do {
            sr = enc->pVtbl->SubmitInput(enc, in);
            if (sr == AMF_INPUT_FULL) { pull_output(enc, &got, &totalVCL, &firstSize, &sawIDR, &sawSEI); Sleep(1); }
        } while (sr == AMF_INPUT_FULL && ++tries < 20);
        if (sr == AMF_OK) submitted++;
        while (pull_output(enc, &got, &totalVCL, &firstSize, &sawIDR, &sawSEI)) { /* забираем всё готовое */ }
    }
    // дренаж хвоста
    enc->pVtbl->Drain(enc);
    for (int i = 0; i < 120; i++) {
        AMFData *o = NULL;
        AMF_RESULT r = enc->pVtbl->QueryOutput(enc, &o);
        if (r == AMF_EOF) break;
        if (r == AMF_OK && o) {
            AMFBuffer *ob = (AMFBuffer *)o;
            int os = (int)ob->pVtbl->GetSize(ob);
            const uint8_t *od = (const uint8_t *)ob->pVtbl->GetNative(ob);
            int vcl = 0, idr = 0, sei = 0;
            if (od && os > 0) count_nals(od, os, &vcl, &idr, &sei);
            totalVCL += vcl; if (idr) sawIDR = 1; if (sei) sawSEI = 1; got++;
            o->pVtbl->Release(o);
        } else {
            Sleep(1);
        }
    }

    double slicesPerFrame = got ? (double)totalVCL / got : 0.0;
    RP(&R, "ok  прогон: submitted=%d AUs=%d firstAU=%dБ slices/frame=%.1f IDR=%d recoverySEI=%d\n",
       submitted, got, firstSize, slicesPerFrame, sawIDR, sawSEI);
    RP(&R, "=========================================================\n");
    RP(&R, "ВЕРДИКТ: AMF из mingw %s\n", got > 0 ? "РАБОТАЕТ" : "НЕ выдал выход");
    RP(&R, "  слайсы в битстриме: %.1f/кадр (MF давал 1.0)\n", slicesPerFrame);
    RP(&R, "  intra-refresh: %s\n", gotIR == irPerSlot ? "принят драйвером" : "НЕ принят");
    RP(&R, "=========================================================\n");

    surf->pVtbl->Release(surf);
    enc->pVtbl->Terminate(enc); enc->pVtbl->Release(enc);
    ctx->pVtbl->Terminate(ctx); ctx->pVtbl->Release(ctx);
    return R.len;
}
