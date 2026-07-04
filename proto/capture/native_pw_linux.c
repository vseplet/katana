//go:build linux && cgo

// Нативный Wayland-захват+энкод в ОДНОМ процессе. Два пути:
//
//   1) DMABUF zero-copy (основной): PipeWire отдаёт кадр как dmabuf-fd (GPU-хендл,
//      без CPU-копии) → импорт в VAAPI через AV_PIX_FMT_DRM_PRIME + hwmap →
//      scale_vaapi=nv12 → h264_vaapi. Энкодим ПО ПРИХОДУ кадра (arrival-driven),
//      без таймера и без дедупа: нет memcpy → нет RT-контеншена → нет overload.
//      Модификаторы буфера согласуем как OBS/gpu-screen-recorder: список берём у
//      EGL (eglQueryDmaBufModifiersEXT), отдаём компаундом с DONT_FIXATE, ждём
//      фиксации, импортируем ровно с тем модификатором.
//
//   2) CPU MemPtr (fallback): старый путь — PipeWire отдаёт BGRx в разделяемой
//      памяти, memcpy последнего кадра, таймер энкодит его ровно на fps (CFR).
//      Включается, если dmabuf не согласовался. Не хуже прежних стабильных 60fps.

#include "native_pw_linux.h"

#include <fcntl.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/pod/builder.h>
#include <spa/pod/iter.h>

#include <libdrm/drm_fourcc.h>
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <gbm.h>

#include <libavcodec/avcodec.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersink.h>
#include <libavfilter/buffersrc.h>
#include <libavutil/hwcontext.h>
#include <libavutil/hwcontext_drm.h>
#include <libavutil/opt.h>

extern void goNativeH264(void *data, int len);
extern void goNativeLog(char *msg); // C-логи → в лог хоста (Go)

#define RENDER_NODE "/dev/dri/renderD128"
#define MAX_MODS 64

static void nlog(const char *fmt, ...)
{
	char buf[512];
	va_list ap;
	va_start(ap, fmt);
	vsnprintf(buf, sizeof(buf), fmt, ap);
	va_end(ap);
	goNativeLog(buf);
}

static long long now_ns(void)
{
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (long long)t.tv_sec * 1000000000LL + t.tv_nsec;
}

struct nstate {
	struct pw_thread_loop *loop;
	struct pw_context *context;
	struct pw_core *core;
	struct pw_stream *stream;
	struct spa_hook stream_listener;
	struct spa_video_info_raw format;

	int cfg_fps, cfg_kbps, cfg_w, cfg_h; // cfg_w/h — целевой (даунскейл), 0 = как есть

	AVBufferRef *hw_device;  // VAAPI (CPU-путь: hwupload)
	AVBufferRef *drm_device; // DRM (dmabuf-путь: источник PRIME-кадров)
	AVBufferRef *drm_frames; // DRM_PRIME frames ctx (для buffersrc dmabuf)
	AVFilterGraph *graph;
	AVFilterContext *src, *sink;
	AVCodecContext *enc;
	int w, h, inited;
	long long pts;

	// Режим захвата, выбранный при согласовании формата.
	int use_dmabuf;       // 1 = dmabuf zero-copy, 0 = CPU MemPtr
	int reneg_done;       // модификаторы уже зафиксировали (анти-цикл)
	uint64_t modifier;    // согласованный DRM-модификатор
	uint32_t drm_fourcc;  // DRM fourcc согласованного формата
	int n_planes;         // число плоскостей dmabuf (для BGRx/BGRA = 1)

	// Единый энкод-поток. CPU-путь: таймер CFR по latest. dmabuf-путь: ждёт
	// свежий PRIME-кадр по condvar и энкодит его сразу (arrival-driven).
	pthread_mutex_t mtx;
	pthread_cond_t cond;
	pthread_t enc_thread;
	int thread_started;

	// CPU-путь: последний BGRx-кадр (memcpy под mtx, энкод вне блокировки).
	uint8_t *latest;
	size_t latest_cap;
	int latest_stride;
	int have;

	// dmabuf-путь: последний PRIME-кадр (обёрнут, fd продублированы). Таймер гонит
	// ЕГО в энкодер ровно на fps (CFR, как Mac-тикер) — на статике переотправляем
	// тот же кадр. Так доставка ровная (фикс. длительность в WriteSample), а RTP-
	// часы не плывут; при этом кадр не ресэмплится (энкодим всегда самый свежий).
	AVFrame *latest_frame;
};

static struct nstate S;
static int g_running;

static void log_averr(const char *what, int err)
{
	char buf[256];
	av_strerror(err, buf, sizeof(buf));
	nlog("%s: %s", what, buf);
}

// --- Соответствие форматов: SPA ↔ DRM fourcc ↔ AVPixelFormat ------------------

static uint32_t spa_to_drm_fourcc(uint32_t f)
{
	switch (f) {
	case SPA_VIDEO_FORMAT_BGRx: return DRM_FORMAT_XRGB8888;
	case SPA_VIDEO_FORMAT_RGBx: return DRM_FORMAT_XBGR8888;
	case SPA_VIDEO_FORMAT_BGRA: return DRM_FORMAT_ARGB8888;
	case SPA_VIDEO_FORMAT_RGBA: return DRM_FORMAT_ABGR8888;
	default: return 0;
	}
}

static enum AVPixelFormat spa_to_av(uint32_t f)
{
	switch (f) {
	case SPA_VIDEO_FORMAT_BGRx: return AV_PIX_FMT_BGR0;
	case SPA_VIDEO_FORMAT_RGBx: return AV_PIX_FMT_RGB0;
	case SPA_VIDEO_FORMAT_BGRA: return AV_PIX_FMT_BGRA;
	case SPA_VIDEO_FORMAT_RGBA: return AV_PIX_FMT_RGBA;
	default: return AV_PIX_FMT_BGR0;
	}
}

// mod_ok: отсекаем AMD-модификаторы с DCC. У них 2 dmabuf-объекта (данные +
// метаданные сжатия), а ffmpeg VAAPI-hwmap умеет мапить только ОДИН DRM-объект
// («VAAPI can only map frames made from a single DRM object»). Отбрасывая их из
// оффера, заставляем компоузитор выбрать одноплоскостной буфер.
#define AMD_FMT_MOD_DCC_SHIFT 13
static int mod_ok(uint64_t m)
{
	uint64_t vendor = m >> 56;
	if (vendor == 0x02 /*DRM_FORMAT_MOD_VENDOR_AMD*/ && ((m >> AMD_FMT_MOD_DCC_SHIFT) & 1))
		return 0;
	return 1;
}

static int filter_mods(uint64_t *m, int n)
{
	int k = 0;
	for (int i = 0; i < n; i++)
		if (mod_ok(m[i]))
			m[k++] = m[i];
	return k;
}

// --- EGL: список dmabuf-модификаторов, которые ДРАЙВЕР умеет импортировать ----
// Тот же приём, что у OBS/gpu-screen-recorder. Открываем render-ноду, headless
// gbm+EGL, спрашиваем eglQueryDmaBufModifiersEXT для нужного fourcc. Компоузитор
// выберет пересечение со своим набором. Провал → [DRM_FORMAT_MOD_INVALID].
static int egl_query_modifiers(uint32_t fourcc, uint64_t *out, int max)
{
	int n = 0;
	int fd = open(RENDER_NODE, O_RDWR | O_CLOEXEC);
	if (fd < 0)
		return 0;
	struct gbm_device *gbm = gbm_create_device(fd);
	if (!gbm) {
		close(fd);
		return 0;
	}
	PFNEGLGETPLATFORMDISPLAYEXTPROC get_dpy =
		(PFNEGLGETPLATFORMDISPLAYEXTPROC)eglGetProcAddress("eglGetPlatformDisplayEXT");
	PFNEGLQUERYDMABUFMODIFIERSEXTPROC query_mods =
		(PFNEGLQUERYDMABUFMODIFIERSEXTPROC)eglGetProcAddress("eglQueryDmaBufModifiersEXT");
	EGLDisplay dpy = EGL_NO_DISPLAY;
	if (get_dpy)
		dpy = get_dpy(EGL_PLATFORM_GBM_KHR, gbm, NULL);
	if (dpy != EGL_NO_DISPLAY && eglInitialize(dpy, NULL, NULL) && query_mods) {
		EGLint cnt = 0;
		if (query_mods(dpy, fourcc, 0, NULL, NULL, &cnt) && cnt > 0) {
			if (cnt > max)
				cnt = max;
			if (query_mods(dpy, fourcc, cnt, out, NULL, &cnt))
				n = filter_mods(out, cnt);
		}
	}
	if (dpy != EGL_NO_DISPLAY)
		eglTerminate(dpy);
	gbm_device_destroy(gbm);
	close(fd);
	return n;
}

// --- Построение SPA-формата с модификаторами ----------------------------------

static struct spa_pod *build_dmabuf_format(struct spa_pod_builder *b, uint32_t fmt,
	uint64_t *mods, int nmods, int dont_fixate, int fps)
{
	struct spa_pod_frame f0, f1;
	spa_pod_builder_push_object(b, &f0, SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat);
	spa_pod_builder_add(b, SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video), 0);
	spa_pod_builder_add(b, SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw), 0);
	spa_pod_builder_add(b, SPA_FORMAT_VIDEO_format, SPA_POD_Id(fmt), 0);
	if (nmods > 0) {
		int flags = SPA_POD_PROP_FLAG_MANDATORY | (dont_fixate ? SPA_POD_PROP_FLAG_DONT_FIXATE : 0);
		spa_pod_builder_prop(b, SPA_FORMAT_VIDEO_modifier, flags);
		spa_pod_builder_push_choice(b, &f1, SPA_CHOICE_Enum, 0);
		spa_pod_builder_long(b, mods[0]); // default
		for (int i = 0; i < nmods; i++)
			spa_pod_builder_long(b, mods[i]);
		spa_pod_builder_pop(b, &f1);
	}
	spa_pod_builder_add(b, SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
		&SPA_RECTANGLE(1920, 1080), &SPA_RECTANGLE(1, 1), &SPA_RECTANGLE(8192, 8192)), 0);
	spa_pod_builder_add(b, SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
		&SPA_FRACTION(fps, 1), &SPA_FRACTION(0, 1), &SPA_FRACTION(240, 1)), 0);
	return spa_pod_builder_pop(b, &f0);
}

static struct spa_pod *build_shm_format(struct spa_pod_builder *b, int fps)
{
	return spa_pod_builder_add_object(b,
		SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(4,
			SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRx,
			SPA_VIDEO_FORMAT_RGBx, SPA_VIDEO_FORMAT_BGRA),
		SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
			&SPA_RECTANGLE(1920, 1080), &SPA_RECTANGLE(1, 1), &SPA_RECTANGLE(8192, 8192)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
			&SPA_FRACTION(fps, 1), &SPA_FRACTION(0, 1), &SPA_FRACTION(240, 1)));
}

// --- Инициализация энкодера ---------------------------------------------------

// init_encoder_cpu: VAAPI-устройство, filtergraph (bgr0 → hwupload → scale_vaapi
// nv12 на GPU) и h264_vaapi. CPU-путь: вход из buffer с BGR0.
static int init_encoder_cpu(int in_w, int in_h)
{
	int ret;
	int out_w = S.cfg_w > 0 ? S.cfg_w : in_w;
	int out_h = S.cfg_h > 0 ? S.cfg_h : in_h;

	if ((ret = av_hwdevice_ctx_create(&S.hw_device, AV_HWDEVICE_TYPE_VAAPI,
			RENDER_NODE, NULL, 0)) < 0) {
		log_averr("hwdevice_ctx_create", ret);
		return -1;
	}

	S.graph = avfilter_graph_alloc();
	char args[512];
	snprintf(args, sizeof(args),
		"video_size=%dx%d:pix_fmt=%d:time_base=1/%d:pixel_aspect=1/1",
		in_w, in_h, AV_PIX_FMT_BGR0, S.cfg_fps);
	if ((ret = avfilter_graph_create_filter(&S.src, avfilter_get_by_name("buffer"),
			"in", args, NULL, S.graph)) < 0) {
		log_averr("buffersrc", ret);
		return -1;
	}
	if ((ret = avfilter_graph_create_filter(&S.sink, avfilter_get_by_name("buffersink"),
			"out", NULL, NULL, S.graph)) < 0) {
		log_averr("buffersink", ret);
		return -1;
	}

	AVFilterInOut *outs = avfilter_inout_alloc();
	AVFilterInOut *ins = avfilter_inout_alloc();
	outs->name = av_strdup("in");
	outs->filter_ctx = S.src;
	outs->pad_idx = 0;
	outs->next = NULL;
	ins->name = av_strdup("out");
	ins->filter_ctx = S.sink;
	ins->pad_idx = 0;
	ins->next = NULL;

	char desc[256];
	snprintf(desc, sizeof(desc),
		"hwupload,scale_vaapi=w=%d:h=%d:format=nv12", out_w, out_h);
	if ((ret = avfilter_graph_parse_ptr(S.graph, desc, &ins, &outs, NULL)) < 0) {
		log_averr("graph_parse", ret);
		avfilter_inout_free(&ins);
		avfilter_inout_free(&outs);
		return -1;
	}
	avfilter_inout_free(&ins);
	avfilter_inout_free(&outs);

	for (unsigned i = 0; i < S.graph->nb_filters; i++) {
		AVFilterContext *f = S.graph->filters[i];
		if (strcmp(f->filter->name, "hwupload") == 0)
			f->hw_device_ctx = av_buffer_ref(S.hw_device);
	}
	if ((ret = avfilter_graph_config(S.graph, NULL)) < 0) {
		log_averr("graph_config", ret);
		return -1;
	}
	return 0;
}

// init_encoder_dmabuf: DRM-устройство + DRM_PRIME frames ctx для входа; graph
// hwmap=derive_device=vaapi (импорт dmabuf в VAAPI, zero-copy) → scale_vaapi nv12.
static int init_encoder_dmabuf(int in_w, int in_h)
{
	int ret;
	int out_w = S.cfg_w > 0 ? S.cfg_w : in_w;
	int out_h = S.cfg_h > 0 ? S.cfg_h : in_h;
	enum AVPixelFormat sw = spa_to_av(S.format.format);

	if ((ret = av_hwdevice_ctx_create(&S.drm_device, AV_HWDEVICE_TYPE_DRM,
			RENDER_NODE, NULL, 0)) < 0) {
		log_averr("drm hwdevice", ret);
		return -1;
	}
	S.drm_frames = av_hwframe_ctx_alloc(S.drm_device);
	if (!S.drm_frames) {
		nlog("drm frames alloc failed");
		return -1;
	}
	AVHWFramesContext *fc = (AVHWFramesContext *)S.drm_frames->data;
	fc->format = AV_PIX_FMT_DRM_PRIME;
	fc->sw_format = sw;
	fc->width = in_w;
	fc->height = in_h;
	if ((ret = av_hwframe_ctx_init(S.drm_frames)) < 0) {
		log_averr("drm frames init", ret);
		return -1;
	}

	S.graph = avfilter_graph_alloc();
	char args[512];
	snprintf(args, sizeof(args),
		"video_size=%dx%d:pix_fmt=%d:time_base=1/%d:pixel_aspect=1/1",
		in_w, in_h, AV_PIX_FMT_DRM_PRIME, S.cfg_fps);
	if ((ret = avfilter_graph_create_filter(&S.src, avfilter_get_by_name("buffer"),
			"in", args, NULL, S.graph)) < 0) {
		log_averr("buffersrc", ret);
		return -1;
	}
	// Привязываем DRM frames ctx к buffersrc (иначе он не примет hw-кадры).
	AVBufferSrcParameters *p = av_buffersrc_parameters_alloc();
	p->format = AV_PIX_FMT_DRM_PRIME;
	p->width = in_w;
	p->height = in_h;
	p->time_base = (AVRational){1, S.cfg_fps};
	p->hw_frames_ctx = av_buffer_ref(S.drm_frames);
	ret = av_buffersrc_parameters_set(S.src, p);
	av_buffer_unref(&p->hw_frames_ctx);
	av_free(p);
	if (ret < 0) {
		log_averr("buffersrc_parameters_set", ret);
		return -1;
	}
	if ((ret = avfilter_graph_create_filter(&S.sink, avfilter_get_by_name("buffersink"),
			"out", NULL, NULL, S.graph)) < 0) {
		log_averr("buffersink", ret);
		return -1;
	}

	AVFilterInOut *outs = avfilter_inout_alloc();
	AVFilterInOut *ins = avfilter_inout_alloc();
	outs->name = av_strdup("in");
	outs->filter_ctx = S.src;
	outs->pad_idx = 0;
	outs->next = NULL;
	ins->name = av_strdup("out");
	ins->filter_ctx = S.sink;
	ins->pad_idx = 0;
	ins->next = NULL;

	// hwmap импортирует PRIME-кадр в VAAPI-поверхность (drm-устройство деривит
	// vaapi, общий fd — zero-copy). Дальше scale_vaapi делает CSC/скейл в nv12.
	char desc[256];
	snprintf(desc, sizeof(desc),
		"hwmap=derive_device=vaapi,scale_vaapi=w=%d:h=%d:format=nv12", out_w, out_h);
	if ((ret = avfilter_graph_parse_ptr(S.graph, desc, &ins, &outs, NULL)) < 0) {
		log_averr("graph_parse", ret);
		avfilter_inout_free(&ins);
		avfilter_inout_free(&outs);
		return -1;
	}
	avfilter_inout_free(&ins);
	avfilter_inout_free(&outs);
	if ((ret = avfilter_graph_config(S.graph, NULL)) < 0) {
		log_averr("graph_config", ret);
		return -1;
	}
	return 0;
}

static void *enc_thread_fn(void *arg);

// init_encoder: общий хвост — открыть h264_vaapi на hw_frames_ctx из sink и
// поднять энкод-поток. graph уже собран (cpu/dmabuf).
static int init_encoder(int in_w, int in_h)
{
	int ret;
	S.w = in_w;
	S.h = in_h;
	int out_w = S.cfg_w > 0 ? S.cfg_w : in_w;
	int out_h = S.cfg_h > 0 ? S.cfg_h : in_h;

	if (S.use_dmabuf)
		ret = init_encoder_dmabuf(in_w, in_h);
	else
		ret = init_encoder_cpu(in_w, in_h);
	if (ret < 0) {
		if (S.use_dmabuf)
			nlog("dmabuf encoder init failed — will not stream (check fallback)");
		return -1;
	}

	const AVCodec *codec = avcodec_find_encoder_by_name("h264_vaapi");
	if (!codec) {
		nlog("h264_vaapi not found");
		return -1;
	}
	S.enc = avcodec_alloc_context3(codec);
	S.enc->width = out_w;
	S.enc->height = out_h;
	S.enc->pix_fmt = AV_PIX_FMT_VAAPI;
	S.enc->time_base = (AVRational){1, S.cfg_fps};
	S.enc->framerate = (AVRational){S.cfg_fps, 1};
	S.enc->bit_rate = (int64_t)S.cfg_kbps * 1000; // из настроек (--bitrate/зритель)
	S.enc->gop_size = S.cfg_fps;
	S.enc->max_b_frames = 0;
	S.enc->refs = 1;
	S.enc->flags |= AV_CODEC_FLAG_LOW_DELAY;
	AVBufferRef *fctx = av_buffersink_get_hw_frames_ctx(S.sink);
	if (!fctx) {
		nlog("no hw_frames_ctx from sink");
		return -1;
	}
	S.enc->hw_frames_ctx = av_buffer_ref(fctx);
	if ((ret = avcodec_open2(S.enc, codec, NULL)) < 0) {
		log_averr("avcodec_open2", ret);
		return -1;
	}
	pthread_mutex_lock(&S.mtx);
	S.inited = 1;
	pthread_mutex_unlock(&S.mtx);
	nlog("encoder ready (%s) in=%dx%d out=%dx%d @%d %dk",
		S.use_dmabuf ? "dmabuf" : "cpu", in_w, in_h, out_w, out_h, S.cfg_fps, S.cfg_kbps);

	if (!S.thread_started) {
		S.thread_started = 1;
		pthread_create(&S.enc_thread, NULL, enc_thread_fn, NULL);
	}
	return 0;
}

static void drain_packets(void)
{
	AVPacket *pkt = av_packet_alloc();
	while (avcodec_receive_packet(S.enc, pkt) == 0) {
		goNativeH264(pkt->data, pkt->size);
		av_packet_unref(pkt);
	}
	av_packet_free(&pkt);
}

// encode_frame: прогоняет готовый AVFrame (BGR0 CPU-кадр или PRIME-кадр) через
// graph → h264_vaapi. force_key=1 — запросить IDR на этом кадре (pict_type=I).
static void encode_frame(AVFrame *in, int force_key)
{
	int ret = av_buffersrc_add_frame_flags(S.src, in, AV_BUFFERSRC_FLAG_KEEP_REF);
	if (ret < 0) {
		log_averr("buffersrc_add", ret);
		return;
	}
	AVFrame *hw = av_frame_alloc();
	while ((ret = av_buffersink_get_frame(S.sink, hw)) >= 0) {
		if (force_key)
			hw->pict_type = AV_PICTURE_TYPE_I;
		ret = avcodec_send_frame(S.enc, hw);
		av_frame_unref(hw);
		if (ret < 0) {
			log_averr("send_frame", ret);
			break;
		}
		drain_packets();
	}
	av_frame_free(&hw);
}

// filter_to_nv12: прогоняет вход через graph (dmabuf → hwmap → scale_vaapi) и
// отдаёт СВОЮ nv12 VAAPI-поверхность (VPP делает копию — она независима от dmabuf-
// буфера KWin). Зовём на ПРИХОДЕ кадра (буфер KWin ещё валиден); после этого его
// можно вернуть. Так таймер потом кодирует стабильную копию, а не гоняемый KWin'ом
// буфер → нет перемешивания/разрыва кадров.
static AVFrame *filter_to_nv12(AVFrame *in)
{
	int ret = av_buffersrc_add_frame_flags(S.src, in, AV_BUFFERSRC_FLAG_KEEP_REF);
	if (ret < 0) {
		log_averr("buffersrc_add", ret);
		return NULL;
	}
	AVFrame *hw = av_frame_alloc();
	if (av_buffersink_get_frame(S.sink, hw) < 0) {
		av_frame_free(&hw);
		return NULL;
	}
	return hw; // nv12 VAAPI-поверхность, наша
}

// encode_surface: гонит готовую nv12-поверхность в h264_vaapi (зовёт таймер).
static void encode_surface(AVFrame *nv12, int force_key)
{
	if (force_key)
		nv12->pict_type = AV_PICTURE_TYPE_I;
	int ret = avcodec_send_frame(S.enc, nv12);
	if (ret < 0) {
		log_averr("send_frame", ret);
		return;
	}
	drain_packets();
}

// --- dmabuf: обёртка spa-буфера в AVFrame(DRM_PRIME) --------------------------

static void drm_desc_free(void *opaque, uint8_t *data)
{
	AVDRMFrameDescriptor *d = (AVDRMFrameDescriptor *)data;
	for (int i = 0; i < d->nb_objects; i++)
		if (d->objects[i].fd >= 0)
			close(d->objects[i].fd);
	av_free(d);
}

// wrap_dmabuf: дублируем fd плоскостей (чтобы PipeWire мог переиспользовать буфер
// сразу), заполняем AVDRMFrameDescriptor и оборачиваем в AVFrame. fd закроются в
// drm_desc_free, когда VAAPI отпустит поверхность.
static AVFrame *wrap_dmabuf(struct spa_buffer *buf)
{
	int planes = (int)buf->n_datas;
	if (planes < 1)
		return NULL;
	if (planes > AV_DRM_MAX_PLANES)
		planes = AV_DRM_MAX_PLANES;

	AVDRMFrameDescriptor *desc = av_mallocz(sizeof(*desc));
	if (!desc)
		return NULL;
	desc->nb_objects = planes;
	desc->nb_layers = 1;
	desc->layers[0].format = S.drm_fourcc;
	desc->layers[0].nb_planes = planes;
	for (int i = 0; i < planes; i++) {
		int fd = dup(buf->datas[i].fd);
		desc->objects[i].fd = fd;
		desc->objects[i].size = buf->datas[i].maxsize;
		desc->objects[i].format_modifier = S.modifier;
		desc->layers[0].planes[i].object_index = i;
		desc->layers[0].planes[i].offset = buf->datas[i].chunk ? buf->datas[i].chunk->offset : 0;
		desc->layers[0].planes[i].pitch = buf->datas[i].chunk ? buf->datas[i].chunk->stride : 0;
	}

	AVFrame *f = av_frame_alloc();
	if (!f) {
		drm_desc_free(NULL, (uint8_t *)desc);
		return NULL;
	}
	f->format = AV_PIX_FMT_DRM_PRIME;
	f->width = S.w;
	f->height = S.h;
	f->buf[0] = av_buffer_create((uint8_t *)desc, sizeof(*desc), drm_desc_free, NULL, 0);
	if (!f->buf[0]) {
		drm_desc_free(NULL, (uint8_t *)desc);
		av_frame_free(&f);
		return NULL;
	}
	f->data[0] = (uint8_t *)desc;
	f->hw_frames_ctx = av_buffer_ref(S.drm_frames);
	return f;
}

// --- Согласование формата -----------------------------------------------------

// renegotiate_fixate: получив от компоузитора список модификаторов (DONT_FIXATE),
// пере-предлагаем ровно его, но БЕЗ DONT_FIXATE — компоузитор зафиксирует один.
static void renegotiate_fixate(uint32_t fmt, uint64_t *mods, int nmods)
{
	uint8_t buffer[4096];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	const struct spa_pod *params[1];
	params[0] = build_dmabuf_format(&b, fmt, mods, nmods, 0 /*fixate*/, S.cfg_fps);
	pw_stream_update_params(S.stream, params, 1);
	nlog("dmabuf: re-offer %d modifiers to fixate", nmods);
}

static void on_param_changed(void *userdata, uint32_t id, const struct spa_pod *param)
{
	(void)userdata;
	if (param == NULL || id != SPA_PARAM_Format)
		return;
	spa_format_video_raw_parse(param, &S.format);

	const struct spa_pod_prop *mod_prop =
		spa_pod_find_prop(param, NULL, SPA_FORMAT_VIDEO_modifier);

	// Шаг фиксации: пришёл нефиксированный список модификаторов — выбираем и
	// пере-предлагаем без DONT_FIXATE, ждём следующий param_changed.
	if (mod_prop && !S.reneg_done && (mod_prop->flags & SPA_POD_PROP_FLAG_DONT_FIXATE)) {
		uint32_t nvals = 0, choice = 0;
		const struct spa_pod *vals = spa_pod_get_values(&mod_prop->value, &nvals, &choice);
		if (vals && nvals >= 1 && vals->type == SPA_TYPE_Long) {
			uint64_t *src = SPA_POD_BODY(vals); // src[0]=default, далее варианты
			uint64_t m[MAX_MODS];
			int n = 0;
			for (uint32_t i = 0; i < nvals && n < MAX_MODS; i++)
				if (mod_ok(src[i])) // выкидываем DCC — их VAAPI не мапит
					m[n++] = src[i];
			if (n > 0) {
				S.reneg_done = 1;
				renegotiate_fixate(S.format.format, m, n);
				return;
			}
		}
	}

	if (S.inited)
		return;

	// Решаем путь: модификатор в формате → dmabuf, иначе CPU MemPtr.
	S.use_dmabuf = (mod_prop != NULL);
	if (S.use_dmabuf) {
		S.modifier = S.format.modifier;
		S.drm_fourcc = spa_to_drm_fourcc(S.format.format);
		S.n_planes = 1;
		if (S.drm_fourcc == 0) {
			nlog("dmabuf: unsupported spa format %d — fallback to cpu", S.format.format);
			S.use_dmabuf = 0;
		}
	}

	// Отвечаем параметрами буферов: тип данных (dmabuf vs shm) и число блоков.
	uint8_t bufbuf[1024];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(bufbuf, sizeof(bufbuf));
	const struct spa_pod *params[1];
	uint32_t types = S.use_dmabuf ? (1u << SPA_DATA_DmaBuf)
		: ((1u << SPA_DATA_MemPtr) | (1u << SPA_DATA_MemFd));
	// dataType — битовая маска обычным Int (как OBS/gsr). blocks НЕ фиксируем: у
	// AMD-модификаторов с DCC бывает 2 плоскости, продюсер задаёт число сам.
	params[0] = spa_pod_builder_add_object(&b,
		SPA_TYPE_OBJECT_ParamBuffers, SPA_PARAM_Buffers,
		SPA_PARAM_BUFFERS_buffers, SPA_POD_CHOICE_RANGE_Int(8, 1, 16),
		SPA_PARAM_BUFFERS_dataType, SPA_POD_Int(types));
	pw_stream_update_params(S.stream, params, 1);

	if (S.format.size.width > 0) {
		if (S.use_dmabuf)
			nlog("dmabuf: negotiated fourcc=0x%08x modifier=0x%llx %dx%d",
				S.drm_fourcc, (unsigned long long)S.modifier,
				S.format.size.width, S.format.size.height);
		if (init_encoder(S.format.size.width, S.format.size.height) < 0 && S.use_dmabuf) {
			// dmabuf-инициализация упала — на этой сессии стрима не будет; в
			// следующий раз (рестарт захвата) отдадим shm-фолбэк.
			nlog("dmabuf init failed");
		}
	}
}

// --- Захват -------------------------------------------------------------------

// on_process (поток PipeWire): вычёрпываем очередь, берём свежий буфер.
//   dmabuf: дублируем fd, оборачиваем в AVFrame, кладём в pending, будим энкодер —
//           и СРАЗУ возвращаем буфер (нет memcpy, нет блокировки на энкод).
//   cpu:    memcpy BGRx в latest (энкодит таймер).
static void on_process(void *userdata)
{
	(void)userdata;
	struct pw_buffer *b, *last = NULL;
	while ((b = pw_stream_dequeue_buffer(S.stream)) != NULL) {
		if (last)
			pw_stream_queue_buffer(S.stream, last);
		last = b;
	}
	if (last == NULL)
		return;
	struct spa_buffer *buf = last->buffer;

	static int dbg_once = 0;
	if (!dbg_once) {
		dbg_once = 1;
		nlog("on_process first: n_datas=%d type=%d fd=%d inited=%d use_dmabuf=%d",
			buf->n_datas, buf->n_datas > 0 ? (int)buf->datas[0].type : -1,
			buf->n_datas > 0 ? (int)buf->datas[0].fd : -1, S.inited, S.use_dmabuf);
	}

	if (S.use_dmabuf) {
		if (S.inited && buf->n_datas > 0 && buf->datas[0].type == SPA_DATA_DmaBuf &&
				buf->datas[0].fd >= 0) {
			// Конвертим dmabuf → СВОЯ nv12-поверхность ПОКА буфер KWin валиден.
			AVFrame *drm = wrap_dmabuf(buf);
			if (drm) {
				AVFrame *nv12 = filter_to_nv12(drm);
				av_frame_free(&drm);
				if (nv12) {
					pthread_mutex_lock(&S.mtx);
					if (S.latest_frame)
						av_frame_free(&S.latest_frame); // держим только самый свежий
					S.latest_frame = nv12;
					pthread_mutex_unlock(&S.mtx);
				}
			}
		}
		pw_stream_queue_buffer(S.stream, last); // буфер KWin уже не нужен (есть копия)
		return;
	}

	// CPU-путь: memcpy последнего BGRx-кадра.
	if (S.h > 0 && buf->n_datas > 0 && buf->datas[0].data != NULL) {
		struct spa_data *sd = &buf->datas[0];
		int stride = (sd->chunk && sd->chunk->stride > 0) ? sd->chunk->stride : S.w * 4;
		size_t need = (size_t)stride * S.h;
		pthread_mutex_lock(&S.mtx);
		if (S.latest_cap < need) {
			free(S.latest);
			S.latest = malloc(need);
			S.latest_cap = S.latest ? need : 0;
		}
		if (S.latest) {
			memcpy(S.latest, sd->data, need);
			S.latest_stride = stride;
			S.have = 1;
		}
		pthread_mutex_unlock(&S.mtx);
	}
	pw_stream_queue_buffer(S.stream, last);
}

// --- Энкод-поток --------------------------------------------------------------

// dmabuf: таймер гонит ПОСЛЕДНИЙ PRIME-кадр в энкодер ровно на cfg_fps (CFR, как
// Mac-тикер). На статике переотправляем тот же кадр → ровный поток с фиксированной
// длительностью в WriteSample (RTP-часы не плывут, зритель не дёргается и не
// сыпется — проверено: Mac-путь с той же моделью работает идеально). memcpy'а нет
// (кадр — dmabuf-хендл), поэтому RT-поток PipeWire не перегружается. IDR форсим по
// wall-clock раз в 1с.
#define KEY_INTERVAL_NS 1000000000LL
static void dmabuf_timer_loop(void)
{
	long long interval = 1000000000LL / (S.cfg_fps > 0 ? S.cfg_fps : 30);
	long long last_key = 0;
	long long stat_ns = 0, next_log = 0;
	int stat_n = 0;
	long long next = now_ns() + interval;
	while (g_running) {
		long long sleep_ns = next - now_ns();
		if (sleep_ns > 0) {
			struct timespec ts = {sleep_ns / 1000000000LL, sleep_ns % 1000000000LL};
			nanosleep(&ts, NULL);
		}
		next += interval;
		if (now_ns() - next > interval) // сильно отстали — ресинк
			next = now_ns() + interval;

		pthread_mutex_lock(&S.mtx);
		AVFrame *f = S.latest_frame ? av_frame_clone(S.latest_frame) : NULL;
		pthread_mutex_unlock(&S.mtx);
		if (!f)
			continue;

		long long t = now_ns();
		int force_key = (last_key == 0) || (t - last_key >= KEY_INTERVAL_NS);
		if (force_key)
			last_key = t;
		f->pts = S.pts++;

		long long t0 = now_ns();
		encode_surface(f, force_key); // кодим стабильную nv12-копию (не буфер KWin)
		av_frame_free(&f);
		stat_ns += now_ns() - t0;
		stat_n++;
		if (now_ns() >= next_log) {
			if (stat_n > 0)
				nlog("dmabuf encode avg %lld ms over %d frames",
					(stat_ns / stat_n) / 1000000, stat_n);
			stat_ns = 0;
			stat_n = 0;
			next_log = now_ns() + 5000000000LL;
		}
	}
}

// cpu: энкодим latest ровно на cfg_fps (CFR, как videorate). Кадр копируем под
// мьютексом, энкод — вне блокировки.
static void cpu_timer_loop(void)
{
	long long interval = 1000000000LL / (S.cfg_fps > 0 ? S.cfg_fps : 30);
	uint8_t *buf = NULL;
	size_t bufcap = 0;
	long long stat_ns = 0, next_log = 0;
	int stat_n = 0;
	long long next = now_ns() + interval;
	while (g_running) {
		long long sleep_ns = next - now_ns();
		if (sleep_ns > 0) {
			struct timespec ts = {sleep_ns / 1000000000LL, sleep_ns % 1000000000LL};
			nanosleep(&ts, NULL);
		}
		next += interval;
		if (now_ns() - next > interval)
			next = now_ns() + interval;

		pthread_mutex_lock(&S.mtx);
		int ready = S.inited && S.have;
		int stride = S.latest_stride, w = S.w, h = S.h;
		if (ready) {
			size_t need = (size_t)stride * h;
			if (bufcap < need) {
				free(buf);
				buf = malloc(need);
				bufcap = buf ? need : 0;
			}
			if (buf)
				memcpy(buf, S.latest, need);
			else
				ready = 0;
		}
		pthread_mutex_unlock(&S.mtx);
		if (!ready)
			continue;

		AVFrame *in = av_frame_alloc();
		in->format = AV_PIX_FMT_BGR0;
		in->width = w;
		in->height = h;
		in->data[0] = buf;
		in->linesize[0] = stride;
		in->pts = S.pts++;
		long long t0 = now_ns();
		encode_frame(in, 0); // CFR 60fps + gop=fps → IDR раз в ~1с сам собой
		av_frame_free(&in);
		stat_ns += now_ns() - t0;
		stat_n++;
		if (now_ns() >= next_log) {
			if (stat_n > 0)
				nlog("cpu encode avg %lld ms over %d frames",
					(stat_ns / stat_n) / 1000000, stat_n);
			stat_ns = 0;
			stat_n = 0;
			next_log = now_ns() + 5000000000LL;
		}
	}
	free(buf);
}

static void *enc_thread_fn(void *arg)
{
	(void)arg;
	if (S.use_dmabuf)
		dmabuf_timer_loop();
	else
		cpu_timer_loop();
	return NULL;
}

static void on_state_changed(void *data, enum pw_stream_state old,
	enum pw_stream_state state, const char *error)
{
	(void)data;
	nlog("stream state: %s -> %s%s%s",
		pw_stream_state_as_string(old), pw_stream_state_as_string(state),
		error ? " err=" : "", error ? error : "");
}

static const struct pw_stream_events stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.state_changed = on_state_changed,
	.param_changed = on_param_changed,
	.process = on_process,
};

int katana_native_start(katana_native_cfg cfg)
{
	memset(&S, 0, sizeof(S));
	S.cfg_fps = cfg.fps > 0 ? cfg.fps : 30;
	S.cfg_kbps = cfg.kbps > 0 ? cfg.kbps : 3000;
	S.cfg_w = cfg.width;
	S.cfg_h = cfg.height;
	pthread_mutex_init(&S.mtx, NULL);
	pthread_cond_init(&S.cond, NULL);
	g_running = 1;

	pw_init(NULL, NULL);
	S.loop = pw_thread_loop_new("katana-native", NULL);
	if (!S.loop)
		return -1;
	S.context = pw_context_new(pw_thread_loop_get_loop(S.loop), NULL, 0);

	pw_thread_loop_lock(S.loop);
	if (pw_thread_loop_start(S.loop) < 0) {
		pw_thread_loop_unlock(S.loop);
		return -1;
	}
	S.core = pw_context_connect_fd(S.context, fcntl(cfg.fd, F_DUPFD_CLOEXEC, 0), NULL, 0);
	if (!S.core) {
		pw_thread_loop_unlock(S.loop);
		return -1;
	}
	S.stream = pw_stream_new(S.core, "katana-native",
		pw_properties_new(
			PW_KEY_MEDIA_TYPE, "Video",
			PW_KEY_MEDIA_CATEGORY, "Capture",
			PW_KEY_MEDIA_ROLE, "Screen", NULL));
	pw_stream_add_listener(S.stream, &S.stream_listener, &stream_events, NULL);

	// EnumFormat: сперва dmabuf-варианты (BGRx/BGRA с модификаторами от EGL), затем
	// shm-фолбэк. Компоузитор выберет первый поддержанный. Нет модификаторов от
	// EGL (нет расширения) → отдаём только shm.
	uint64_t mods_bgrx[MAX_MODS], mods_bgra[MAX_MODS];
	int n_bgrx = egl_query_modifiers(DRM_FORMAT_XRGB8888, mods_bgrx, MAX_MODS);
	int n_bgra = egl_query_modifiers(DRM_FORMAT_ARGB8888, mods_bgra, MAX_MODS);
	nlog("egl modifiers: BGRx=%d BGRA=%d", n_bgrx, n_bgra);

	uint8_t buffer[8192];
	struct spa_pod_builder pb = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	const struct spa_pod *params[3];
	int np = 0;
	if (n_bgrx > 0)
		params[np++] = build_dmabuf_format(&pb, SPA_VIDEO_FORMAT_BGRx, mods_bgrx, n_bgrx, 1, cfg.fps > 0 ? cfg.fps : 30);
	if (n_bgra > 0)
		params[np++] = build_dmabuf_format(&pb, SPA_VIDEO_FORMAT_BGRA, mods_bgra, n_bgra, 1, cfg.fps > 0 ? cfg.fps : 30);
	params[np++] = build_shm_format(&pb, cfg.fps > 0 ? cfg.fps : 30);

	// НЕ ставим PW_STREAM_FLAG_MAP_BUFFERS: с dmabuf он пытается mmap'ить буфер и
	// стрим не доходит до streaming. Для dmabuf работаем с fd напрямую; для shm-
	// фолбэка мапим сами при необходимости.
	pw_stream_connect(S.stream, PW_DIRECTION_INPUT, cfg.node,
		PW_STREAM_FLAG_AUTOCONNECT, params, np);
	pw_thread_loop_unlock(S.loop);

	while (g_running) {
		struct timespec ts = {0, 100 * 1000 * 1000};
		nanosleep(&ts, NULL);
	}

	if (S.thread_started)
		pthread_join(S.enc_thread, NULL); // таймер сам выйдет по g_running

	pw_thread_loop_lock(S.loop);
	if (S.stream)
		pw_stream_destroy(S.stream);
	if (S.core)
		pw_core_disconnect(S.core);
	pw_context_destroy(S.context);
	pw_thread_loop_unlock(S.loop);
	pw_thread_loop_destroy(S.loop);

	if (S.enc)
		avcodec_free_context(&S.enc);
	if (S.graph)
		avfilter_graph_free(&S.graph);
	if (S.latest_frame)
		av_frame_free(&S.latest_frame);
	if (S.drm_frames)
		av_buffer_unref(&S.drm_frames);
	if (S.drm_device)
		av_buffer_unref(&S.drm_device);
	if (S.hw_device)
		av_buffer_unref(&S.hw_device);
	free(S.latest);
	pthread_cond_destroy(&S.cond);
	pthread_mutex_destroy(&S.mtx);
	return 0;
}

void katana_native_stop(void)
{
	g_running = 0;
}
