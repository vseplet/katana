//go:build linux && cgo

// Шаг A: PipeWire-консьюмер — подключиться по fd от портала, взять поток по node,
// дождаться одного кадра и вернуть его формат (DMABUF/память, spa-формат,
// модификатор, размер). Нужен, чтобы точно знать, что импортировать в VAAPI.
//
// Собирается только в cgo-сборке под Linux (CFLAGS/LDFLAGS — из pkg-config
// libpipewire-0.3 в video_wayland_native_linux.go).

#include "native_pw_linux.h"

#include <fcntl.h>
#include <time.h>

#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>

struct probe_data {
	struct pw_thread_loop *loop;
	struct pw_context *context;
	struct pw_core *core;
	struct pw_stream *stream;
	struct spa_hook stream_listener;
	struct spa_video_info_raw format;
	katana_frame_info *out;
	int done;
};

static void on_param_changed(void *userdata, uint32_t id, const struct spa_pod *param)
{
	struct probe_data *d = userdata;
	if (param == NULL || id != SPA_PARAM_Format)
		return;
	spa_format_video_raw_parse(param, &d->format);
	d->out->spa_format = d->format.format;
	d->out->modifier = d->format.modifier;
	d->out->width = d->format.size.width;
	d->out->height = d->format.size.height;
}

static void on_process(void *userdata)
{
	struct probe_data *d = userdata;
	struct pw_buffer *b = pw_stream_dequeue_buffer(d->stream);
	if (b == NULL)
		return;
	struct spa_buffer *buf = b->buffer;
	if (!d->done && buf->n_datas > 0) {
		struct spa_data *sd = &buf->datas[0];
		d->out->is_dmabuf = (sd->type == SPA_DATA_DmaBuf) ? 1 : 0;
		d->out->n_planes = buf->n_datas;
		d->out->ok = 1;
		d->done = 1;
		pw_thread_loop_signal(d->loop, false);
	}
	pw_stream_queue_buffer(d->stream, b);
}

static const struct pw_stream_events stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.param_changed = on_param_changed,
	.process = on_process,
};

int katana_pw_probe(int fd, unsigned int node, katana_frame_info *out)
{
	struct probe_data d;
	spa_zero(d);
	d.out = out;
	out->ok = 0;

	pw_init(NULL, NULL);
	d.loop = pw_thread_loop_new("katana-pw", NULL);
	if (d.loop == NULL)
		return -1;
	d.context = pw_context_new(pw_thread_loop_get_loop(d.loop), NULL, 0);

	pw_thread_loop_lock(d.loop);
	if (pw_thread_loop_start(d.loop) < 0) {
		pw_thread_loop_unlock(d.loop);
		return -1;
	}
	// Портальный fd одноразовый у вызывающего — дублируем, PipeWire им владеет.
	d.core = pw_context_connect_fd(d.context, fcntl(fd, F_DUPFD_CLOEXEC, 0), NULL, 0);
	if (d.core == NULL) {
		pw_thread_loop_unlock(d.loop);
		return -1;
	}

	d.stream = pw_stream_new(d.core, "katana-probe",
		pw_properties_new(
			PW_KEY_MEDIA_TYPE, "Video",
			PW_KEY_MEDIA_CATEGORY, "Capture",
			PW_KEY_MEDIA_ROLE, "Screen",
			NULL));
	pw_stream_add_listener(d.stream, &d.stream_listener, &stream_events, &d);

	uint8_t buffer[1024];
	struct spa_pod_builder pb = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	const struct spa_pod *params[1];
	params[0] = spa_pod_builder_add_object(&pb,
		SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(5,
			SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRx,
			SPA_VIDEO_FORMAT_RGBx, SPA_VIDEO_FORMAT_BGRA,
			SPA_VIDEO_FORMAT_RGBA),
		SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
			&SPA_RECTANGLE(1920, 1080),
			&SPA_RECTANGLE(1, 1),
			&SPA_RECTANGLE(8192, 8192)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
			&SPA_FRACTION(30, 1),
			&SPA_FRACTION(0, 1),
			&SPA_FRACTION(240, 1)));

	pw_stream_connect(d.stream, PW_DIRECTION_INPUT, node,
		PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS,
		params, 1);
	pw_thread_loop_unlock(d.loop);

	// Ждём кадр до ~4 секунд.
	pw_thread_loop_lock(d.loop);
	struct timespec ts;
	clock_gettime(CLOCK_REALTIME, &ts);
	ts.tv_sec += 4;
	if (!d.done)
		pw_thread_loop_timed_wait_full(d.loop, &ts);
	pw_thread_loop_unlock(d.loop);

	pw_thread_loop_lock(d.loop);
	pw_stream_destroy(d.stream);
	pw_core_disconnect(d.core);
	pw_context_destroy(d.context);
	pw_thread_loop_unlock(d.loop);
	pw_thread_loop_destroy(d.loop);

	return out->ok ? 0 : -1;
}
