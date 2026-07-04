//go:build linux && cgo

#ifndef KATANA_NATIVE_PW_H
#define KATANA_NATIVE_PW_H

// Нативный Wayland-захват+энкод в одном процессе: PipeWire (BGRx-кадры) →
// libav filtergraph (format=nv12,hwupload) → h264_vaapi (GPU) → Annex-B H264
// в Go-канал (через экспорт goNativeH264). Без gst и без межпроцессного пайпа.

typedef struct {
	int fd;            // PipeWire fd (из портала; C дублирует внутри)
	unsigned int node; // id ScreenCast-потока
	int width, height; // целевой размер энкода
	int fps;
	int kbps;          // целевой битрейт
	int render_fd_hint; // не используется; зарезервировано
} katana_native_cfg;

// Запускает захват+энкод; блокирует до katana_native_stop(). 0 — ок, <0 — ошибка
// инициализации. Каждый H264-пакет отдаётся через goNativeH264(data,len).
int katana_native_start(katana_native_cfg cfg);

// Останавливает захват (можно звать из другого потока).
void katana_native_stop(void);

// Форсит IDR на ближайшем кадре (ответ на PLI зрителя — мгновенное восстановление
// после потерь). Можно звать из любого потока.
void katana_native_force_key(void);

// Меняет битрейт на лету, kbps (адаптация к сети): энкодер переоткрывается на
// ближайшем такте, следом IDR. Можно звать из любого потока.
void katana_native_set_bitrate(int kbps);

#endif
