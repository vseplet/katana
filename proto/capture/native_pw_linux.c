//go:build linux && cgo

// Нативный Wayland-захват+энкод в ОДНОМ процессе: PipeWire (BGRx CPU-кадры) →
// libav filtergraph (format=nv12 → hwupload в VAAPI) → h264_vaapi (GPU) →
// Annex-B H264 наружу в Go (goNativeH264). Без gst и без межпроцессного пайпа.
//
// Собирается только в cgo-сборке под Linux (флаги из pkg-config libpipewire-0.3
// libavcodec libavfilter libavutil в video_wayland_native_linux.go).

#include "native_pw_linux.h"

#include <fcntl.h>
#include <string.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>

#include <libavcodec/avcodec.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersink.h>
#include <libavfilter/buffersrc.h>
#include <libavutil/hwcontext.h>
#include <libavutil/opt.h>
#include <libavutil/pixdesc.h>

// Экспорт из Go: отдать один H264 access unit (Annex-B).
extern void goNativeH264(void *data, int len);

struct nstate {
	struct pw_thread_loop *loop;
	struct pw_context *context;
	struct pw_core *core;
	struct pw_stream *stream;
	struct spa_hook stream_listener;
	struct spa_video_info_raw format;

	int cfg_fps, cfg_kbps;

	// libav
	AVBufferRef *hw_device;
	AVFilterGraph *graph;
	AVFilterContext *src, *sink;
	AVCodecContext *enc;
	int inited;
	long long pts;
};

static struct nstate S;
static int g_running;

static void log_averr(const char *what, int err)
{
	char buf[256];
	av_strerror(err, buf, sizeof(buf));
	pw_log_error("katana native: %s: %s", what, buf);
	fprintf(stderr, "katana native: %s: %s\n", what, buf);
}

// init_encoder строит VAAPI-устройство, filtergraph (bgr0→nv12→hwupload) и
// открывает h264_vaapi. Ленивая инициализация — знаем размер из param_changed.
static int init_encoder(int w, int h)
{
	int ret;
	if ((ret = av_hwdevice_ctx_create(&S.hw_device, AV_HWDEVICE_TYPE_VAAPI,
			"/dev/dri/renderD128", NULL, 0)) < 0) {
		log_averr("hwdevice_ctx_create", ret);
		return -1;
	}

	S.graph = avfilter_graph_alloc();
	if (!S.graph)
		return -1;

	char args[512];
	snprintf(args, sizeof(args),
		"video_size=%dx%d:pix_fmt=%d:time_base=1/%d:pixel_aspect=1/1",
		w, h, AV_PIX_FMT_BGR0, S.cfg_fps);
	if ((ret = avfilter_graph_create_filter(&S.src,
			avfilter_get_by_name("buffer"), "in", args, NULL, S.graph)) < 0) {
		log_averr("create buffersrc", ret);
		return -1;
	}
	if ((ret = avfilter_graph_create_filter(&S.sink,
			avfilter_get_by_name("buffersink"), "out", NULL, NULL, S.graph)) < 0) {
		log_averr("create buffersink", ret);
		return -1;
	}

	// bgr0 → nv12 (CPU) → hwupload (в VAAPI). hwupload берёт устройство из графа.
	AVFilterInOut *outputs = avfilter_inout_alloc();
	AVFilterInOut *inputs = avfilter_inout_alloc();
	outputs->name = av_strdup("in");
	outputs->filter_ctx = S.src;
	outputs->pad_idx = 0;
	outputs->next = NULL;
	inputs->name = av_strdup("out");
	inputs->filter_ctx = S.sink;
	inputs->pad_idx = 0;
	inputs->next = NULL;

	if ((ret = avfilter_graph_parse_ptr(S.graph,
			"format=nv12,hwupload", &inputs, &outputs, NULL)) < 0) {
		log_averr("graph_parse", ret);
		avfilter_inout_free(&inputs);
		avfilter_inout_free(&outputs);
		return -1;
	}
	avfilter_inout_free(&inputs);
	avfilter_inout_free(&outputs);

	// hwupload-фильтру нужен hw_device_ctx.
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
		fprintf(stderr, "katana native: h264_vaapi encoder not found\n");
		return -1;
	}
	S.enc = avcodec_alloc_context3(codec);
	S.enc->width = w;
	S.enc->height = h;
	S.enc->pix_fmt = AV_PIX_FMT_VAAPI;
	S.enc->time_base = (AVRational){1, S.cfg_fps};
	S.enc->framerate = (AVRational){S.cfg_fps, 1};
	S.enc->bit_rate = (int64_t)S.cfg_kbps * 1000;
	S.enc->gop_size = S.cfg_fps;
	S.enc->max_b_frames = 0;
	// hw_frames_ctx энкодера = выход buffersink (VAAPI NV12).
	AVBufferRef *frames_ref = av_buffersink_get_hw_frames_ctx(S.sink);
	if (!frames_ref) {
		fprintf(stderr, "katana native: no hw_frames_ctx from buffersink\n");
		return -1;
	}
	S.enc->hw_frames_ctx = av_buffer_ref(frames_ref);

	if ((ret = avcodec_open2(S.enc, codec, NULL)) < 0) {
		log_averr("avcodec_open2", ret);
		return -1;
	}
	S.inited = 1;
	fprintf(stderr, "katana native: encoder ready %dx%d @%d %dk\n", w, h, S.cfg_fps, S.cfg_kbps);
	return 0;
}

// encode_bgrx кодирует один BGRx-кадр (data/stride) и отдаёт H264 в Go.
static void encode_bgrx(uint8_t *data, int stride, int w, int h)
{
	AVFrame *in = av_frame_alloc();
	in->format = AV_PIX_FMT_BGR0;
	in->width = w;
	in->height = h;
	in->data[0] = data;
	in->linesize[0] = stride;
	in->pts = S.pts++;

	int ret = av_buffersrc_add_frame_flags(S.src, in, AV_BUFFERSRC_FLAG_KEEP_REF);
	av_frame_free(&in);
	if (ret < 0) {
		log_averr("buffersrc_add_frame", ret);
		return;
	}

	AVFrame *hw = av_frame_alloc();
	while ((ret = av_buffersink_get_frame(S.sink, hw)) >= 0) {
		hw->pict_type = 0; // авто
		ret = avcodec_send_frame(S.enc, hw);
		av_frame_unref(hw);
		if (ret < 0) {
			log_averr("send_frame", ret);
			break;
		}
		AVPacket *pkt = av_packet_alloc();
		while (avcodec_receive_packet(S.enc, pkt) == 0) {
			goNativeH264(pkt->data, pkt->size);
			av_packet_unref(pkt);
		}
		av_packet_free(&pkt);
	}
	av_frame_free(&hw);
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

static void on_process(void *userdata)
{
	(void)userdata;
	struct pw_buffer *b = pw_stream_dequeue_buffer(S.stream);
	if (b == NULL)
		return;
	struct spa_buffer *buf = b->buffer;
	if (S.inited && buf->n_datas > 0 && buf->datas[0].data != NULL) {
		struct spa_data *sd = &buf->datas[0];
		int stride = sd->chunk->stride > 0 ? sd->chunk->stride : (int)S.format.size.width * 4;
		encode_bgrx((uint8_t *)sd->data, stride,
			S.format.size.width, S.format.size.height);
	}
	pw_stream_queue_buffer(S.stream, b);
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

	// Крутимся, пока не попросят стоп (кадры обрабатываются в on_process на
	// потоке PipeWire).
	while (g_running) {
		struct timespec ts = {0, 100 * 1000 * 1000}; // 100 мс
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
