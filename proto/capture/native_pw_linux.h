//go:build linux && cgo

#ifndef KATANA_NATIVE_PW_H
#define KATANA_NATIVE_PW_H

// Инфо о первом кадре из PipeWire-потока (шаг A — зонд формата).
typedef struct {
	int ok;                    // 1 — кадр получен
	int is_dmabuf;             // 1 — DMABUF (GPU), 0 — память (shm/ptr)
	unsigned int spa_format;   // SPA video format id
	unsigned long long modifier;
	int width, height;
	int n_planes;
} katana_frame_info;

// Подключиться к PipeWire по fd (из портала), взять поток node, дождаться одного
// кадра, заполнить out и отключиться. 0 — ок, -1 — не дождались/ошибка.
int katana_pw_probe(int fd, unsigned int node, katana_frame_info *out);

#endif
