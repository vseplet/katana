//go:build linux && cgo

// Нативный Wayland-захват+энкод в ОДНОМ процессе: PipeWire (BGRx CPU-кадры) →
// sws_scale в NV12 → заливка в VAAPI-сурфейс → h264_vaapi (GPU) → Annex-B H264
// наружу в Go (goNativeH264). Без gst и без межпроцессного пайпа. Использованы
// только стабильные libav-API (есть в ffmpeg 5.x…8) для портативности.

#include "native_pw_linux.h"

#include <fcntl.h>
#include <string.h>
#include <time.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>

#include <libavcodec/avcodec.h>
#include <libavutil/hwcontext.h>
#include <libavutil/imgutils.h>
#include <libavutil/pixdesc.h>
#include <libswscale/swscale.h>

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

	AVBufferRef *hw_device;
	AVBufferRef *hw_frames;
	struct SwsContext *sws;
	AVCodecContext *enc;
	int w, h, inited;
	long long pts;
};

static struct nstate S;
static int g_running;

static void log_averr(const char *what, int err)
{
	char buf[256];
	av_strerror(err, buf, sizeof(buf));
	fprintf(stderr, "katana native: %s: %s\n", what, buf);
}

// init_encoder: VAAPI-устройство, пул VAAPI-сурфейсов (NV12), sws (BGR0→NV12) и
// h264_vaapi. Ленивая инициализация — размер знаем из param_changed.
static int init_encoder(int w, int h)
{
	int ret;
	S.w = w;
	S.h = h;

	if ((ret = av_hwdevice_ctx_create(&S.hw_device, AV_HWDEVICE_TYPE_VAAPI,
			"/dev/dri/renderD128", NULL, 0)) < 0) {
		log_averr("hwdevice_ctx_create", ret);
		return -1;
	}

	S.hw_frames = av_hwframe_ctx_alloc(S.hw_device);
	if (!S.hw_frames)
		return -1;
	AVHWFramesContext *fc = (AVHWFramesContext *)S.hw_frames->data;
	fc->format = AV_PIX_FMT_VAAPI;
	fc->sw_format = AV_PIX_FMT_NV12;
	fc->width = w;
	fc->height = h;
	fc->initial_pool_size = 20;
	if ((ret = av_hwframe_ctx_init(S.hw_frames)) < 0) {
		log_averr("hwframe_ctx_init", ret);
		return -1;
	}

	S.sws = sws_getContext(w, h, AV_PIX_FMT_BGR0, w, h, AV_PIX_FMT_NV12,
		SWS_BILINEAR, NULL, NULL, NULL);
	if (!S.sws) {
		fprintf(stderr, "katana native: sws_getContext failed\n");
		return -1;
	}

	const AVCodec *codec = avcodec_find_encoder_by_name("h264_vaapi");
	if (!codec) {
		fprintf(stderr, "katana native: h264_vaapi not found\n");
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
	S.enc->hw_frames_ctx = av_buffer_ref(S.hw_frames);
	if ((ret = avcodec_open2(S.enc, codec, NULL)) < 0) {
		log_averr("avcodec_open2", ret);
		return -1;
	}
	S.inited = 1;
	fprintf(stderr, "katana native: encoder ready %dx%d @%d %dk\n", w, h, S.cfg_fps, S.cfg_kbps);
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

// encode_bgrx: BGRx (CPU) → NV12 (CPU, sws) → VAAPI-сурфейс (upload) → энкод.
static void encode_bgrx(uint8_t *data, int stride, int w, int h)
{
	int ret;
	AVFrame *nv12 = av_frame_alloc();
	nv12->format = AV_PIX_FMT_NV12;
	nv12->width = w;
	nv12->height = h;
	if ((ret = av_frame_get_buffer(nv12, 0)) < 0) {
		log_averr("nv12 get_buffer", ret);
		av_frame_free(&nv12);
		return;
	}
	const uint8_t *src[4] = {data, NULL, NULL, NULL};
	int srcStride[4] = {stride, 0, 0, 0};
	sws_scale(S.sws, src, srcStride, 0, h, nv12->data, nv12->linesize);

	AVFrame *hw = av_frame_alloc();
	if ((ret = av_hwframe_get_buffer(S.hw_frames, hw, 0)) < 0) {
		log_averr("hwframe_get_buffer", ret);
		av_frame_free(&nv12);
		av_frame_free(&hw);
		return;
	}
	if ((ret = av_hwframe_transfer_data(hw, nv12, 0)) < 0) {
		log_averr("hwframe_transfer", ret);
		av_frame_free(&nv12);
		av_frame_free(&hw);
		return;
	}
	hw->pts = S.pts++;
	av_frame_free(&nv12);

	ret = avcodec_send_frame(S.enc, hw);
	av_frame_free(&hw);
	if (ret < 0) {
		log_averr("send_frame", ret);
		return;
	}
	drain_packets();
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
		int stride = (sd->chunk && sd->chunk->stride > 0) ? sd->chunk->stride : S.w * 4;
		encode_bgrx((uint8_t *)sd->data, stride, S.w, S.h);
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

	if (S.enc)
		avcodec_free_context(&S.enc);
	if (S.sws)
		sws_freeContext(S.sws);
	if (S.hw_frames)
		av_buffer_unref(&S.hw_frames);
	if (S.hw_device)
		av_buffer_unref(&S.hw_device);
	return 0;
}

void katana_native_stop(void)
{
	g_running = 0;
}
