//go:build linux && cgo

// Нативный Wayland-захват+энкод в ОДНОМ процессе: PipeWire (BGRx CPU-кадры) →
// libav filtergraph (hwupload → scale_vaapi=format=nv12, всё на GPU) → h264_vaapi
// (GPU) → Annex-B H264 наружу в Go (goNativeH264). Без gst и без межпроцессного
// пайпа.
//
// Схема потоков: on_process — realtime-поток PipeWire, у него ЖЁСТКИЙ дедлайн,
// поэтому там НЕЛЬЗЯ кодировать (VAAPI-сабмит не укладывается → PipeWire кричит
// overload и режет частоту вдвое). on_process только копирует свежий кадр в
// S.latest и будит энкод-поток (condvar). Энкод-поток просыпается ПО ПРИХОДУ
// кадра (а не по свободному таймеру) — значит синхронен с компоузитором (нет
// джаддера), кодирует строго по порядку (нет переупорядочивания), и работает вне
// RT-потока (нет overload). Если кадры приходят быстрее энкода — держим только
// свежий (промежуточные отбрасываем, латенси не копится).

#include "native_pw_linux.h"

#include <fcntl.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>

#include <libavcodec/avcodec.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersink.h>
#include <libavfilter/buffersrc.h>
#include <libavutil/hwcontext.h>
#include <libavutil/opt.h>

extern void goNativeH264(void *data, int len);
extern void goNativeLog(char *msg);

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

	AVBufferRef *hw_device;
	AVFilterGraph *graph;
	AVFilterContext *src, *sink;
	AVCodecContext *enc;
	int w, h, inited;
	long long pts;

	// Двойной буфер между RT-потоком (продюсер) и энкод-потоком (консюмер).
	pthread_t enc_thread;
	pthread_mutex_t mtx;
	pthread_cond_t cond;
	uint8_t *latest;   // свежий кадр из on_process
	int latest_cap;    // выделенная ёмкость latest
	int latest_stride; // stride свежего кадра
	int have;          // есть новый кадр для энкода
	int stop_enc;      // сигнал энкод-потоку выйти

	// статистика энкода
	long long stat_ns, next_log;
	int stat_n;
};

static struct nstate S;
static int g_running;

static void log_averr(const char *what, int err)
{
	char buf[256];
	av_strerror(err, buf, sizeof(buf));
	nlog("%s: %s", what, buf);
}

// init_encoder: VAAPI-устройство, filtergraph (bgr0 → hwupload → scale_vaapi nv12
// на GPU) и h264_vaapi. Ленивая инициализация — размер входа из param_changed.
static int init_encoder(int in_w, int in_h)
{
	int ret;
	S.w = in_w;
	S.h = in_h;
	int out_w = S.cfg_w > 0 ? S.cfg_w : in_w;
	int out_h = S.cfg_h > 0 ? S.cfg_h : in_h;

	if ((ret = av_hwdevice_ctx_create(&S.hw_device, AV_HWDEVICE_TYPE_VAAPI,
			"/dev/dri/renderD128", NULL, 0)) < 0) {
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
	S.enc->bit_rate = (int64_t)S.cfg_kbps * 1000;
	S.enc->gop_size = S.cfg_fps;
	S.enc->max_b_frames = 0;
	S.enc->refs = 1;
	S.enc->flags |= AV_CODEC_FLAG_LOW_DELAY; // без переупорядочивания вывода
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
	S.inited = 1;
	nlog("encoder ready in=%dx%d out=%dx%d @%d %dk",
		in_w, in_h, out_w, out_h, S.cfg_fps, S.cfg_kbps);
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

// encode_bgrx: BGRx (CPU) → буфер-src → hwupload+scale_vaapi (GPU) → h264_vaapi.
static void encode_bgrx(uint8_t *data, int stride, int w, int h)
{
	int ret;
	AVFrame *in = av_frame_alloc();
	in->format = AV_PIX_FMT_BGR0;
	in->width = w;
	in->height = h;
	in->data[0] = data;
	in->linesize[0] = stride;
	in->pts = S.pts++;

	ret = av_buffersrc_add_frame_flags(S.src, in, AV_BUFFERSRC_FLAG_KEEP_REF);
	av_frame_free(&in);
	if (ret < 0) {
		log_averr("buffersrc_add", ret);
		return;
	}

	AVFrame *hw = av_frame_alloc();
	while ((ret = av_buffersink_get_frame(S.sink, hw)) >= 0) {
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

// enc_thread_fn: консюмер. Спит на condvar; просыпается КОГДА on_process положил
// свежий кадр (have=1). Копирует его под мьютексом в локальный буфер и кодирует
// уже вне блокировки. Одна побудка = один энкод свежего кадра → синхронно с
// компоузитором, по порядку, без RT-overload.
static void *enc_thread_fn(void *arg)
{
	(void)arg;
	uint8_t *local = NULL;
	int local_cap = 0;
	for (;;) {
		pthread_mutex_lock(&S.mtx);
		while (!S.have && !S.stop_enc)
			pthread_cond_wait(&S.cond, &S.mtx);
		if (S.stop_enc) {
			pthread_mutex_unlock(&S.mtx);
			break;
		}
		int stride = S.latest_stride;
		int need = stride * S.h;
		if (local_cap < need) {
			free(local);
			local = malloc(need);
			local_cap = need;
		}
		memcpy(local, S.latest, need);
		S.have = 0;
		pthread_mutex_unlock(&S.mtx);

		if (!S.inited)
			continue;
		long long t0 = now_ns();
		encode_bgrx(local, stride, S.w, S.h);
		S.stat_ns += now_ns() - t0;
		S.stat_n++;
		if (now_ns() >= S.next_log) {
			if (S.stat_n > 0)
				nlog("encode avg %lld ms over %d frames", (S.stat_ns / S.stat_n) / 1000000, S.stat_n);
			S.stat_ns = 0;
			S.stat_n = 0;
			S.next_log = now_ns() + 5000000000LL;
		}
	}
	free(local);
	return NULL;
}

// on_process (RT-поток PipeWire): вычёрпываем очередь до свежего кадра, копируем
// его в S.latest и будим энкод-поток. Никакого энкода здесь — только memcpy.
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
	if (S.inited && buf->n_datas > 0 && buf->datas[0].data != NULL) {
		struct spa_data *sd = &buf->datas[0];
		int stride = (sd->chunk && sd->chunk->stride > 0) ? sd->chunk->stride : S.w * 4;
		int need = stride * S.h;
		pthread_mutex_lock(&S.mtx);
		if (S.latest_cap < need) {
			free(S.latest);
			S.latest = malloc(need);
			S.latest_cap = need;
		}
		memcpy(S.latest, sd->data, need);
		S.latest_stride = stride;
		S.have = 1;
		pthread_cond_signal(&S.cond);
		pthread_mutex_unlock(&S.mtx);
	}
	pw_stream_queue_buffer(S.stream, last);
}

static void on_param_changed(void *userdata, uint32_t id, const struct spa_pod *param)
{
	(void)userdata;
	if (param == NULL || id != SPA_PARAM_Format)
		return;
	spa_format_video_raw_parse(param, &S.format);
	if (!S.inited && S.format.size.width > 0)
		init_encoder(S.format.size.width, S.format.size.height);
}

static const struct pw_stream_events stream_events = {
	PW_VERSION_STREAM_EVENTS,
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
	g_running = 1;
	pthread_mutex_init(&S.mtx, NULL);
	pthread_cond_init(&S.cond, NULL);
	pthread_create(&S.enc_thread, NULL, enc_thread_fn, NULL);

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

	uint8_t buffer[1024];
	struct spa_pod_builder pb = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	const struct spa_pod *params[1];
	params[0] = spa_pod_builder_add_object(&pb,
		SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(4,
			SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRx,
			SPA_VIDEO_FORMAT_RGBx, SPA_VIDEO_FORMAT_BGRA),
		SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
			&SPA_RECTANGLE(1920, 1080), &SPA_RECTANGLE(1, 1), &SPA_RECTANGLE(8192, 8192)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
			&SPA_FRACTION(cfg.fps > 0 ? cfg.fps : 30, 1), &SPA_FRACTION(0, 1), &SPA_FRACTION(240, 1)));

	pw_stream_connect(S.stream, PW_DIRECTION_INPUT, cfg.node,
		PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS, params, 1);
	pw_thread_loop_unlock(S.loop);

	while (g_running) {
		struct timespec ts = {0, 100 * 1000 * 1000};
		nanosleep(&ts, NULL);
	}

	pw_thread_loop_lock(S.loop);
	if (S.stream)
		pw_stream_destroy(S.stream);
	if (S.core)
		pw_core_disconnect(S.core);
	pw_context_destroy(S.context);
	pw_thread_loop_unlock(S.loop);
	pw_thread_loop_destroy(S.loop);

	// Останавливаем энкод-поток.
	pthread_mutex_lock(&S.mtx);
	S.stop_enc = 1;
	pthread_cond_signal(&S.cond);
	pthread_mutex_unlock(&S.mtx);
	pthread_join(S.enc_thread, NULL);
	pthread_mutex_destroy(&S.mtx);
	pthread_cond_destroy(&S.cond);
	free(S.latest);

	if (S.enc)
		avcodec_free_context(&S.enc);
	if (S.graph)
		avfilter_graph_free(&S.graph);
	if (S.hw_device)
		av_buffer_unref(&S.hw_device);
	return 0;
}

void katana_native_stop(void)
{
	g_running = 0;
}
