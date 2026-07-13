#ifndef KATANA_AMF_ENCODER_H
#define KATANA_AMF_ENCODER_H

#include <stdint.h>

// Нативный аппаратный H264-энкодер поверх AMD AMF SDK (в обход Media Foundation).
// Смысл существования: AMF даёт слайсы и rolling intra-refresh, которые MF-обёртка AMD
// молча игнорит (проверено: MF = 1.0 slices/frame, ir=IGNORED; AMF реально режет на
// слайсы и принимает intra-refresh — локализация потерь без ожидания кейфрейма/NACK).
//
// Интерфейс 1:1 повторяет mf_encoder_windows.h, чтобы встать за тем же Go-интерфейсом
// winVideoEncoder. Рантайм amfrt64.dll грузится динамически (он в драйвере AMD); если
// его нет (не-AMD / старый драйвер) — katana_amf_available() вернёт 0, и Go откатится
// на MF-энкодер. Хедеры AMF завендорены в proto/thirdparty/amf (MIT).

typedef struct katana_amf katana_amf;

// katana_amf_available — есть ли рантайм AMF (amfrt64.dll грузится). Дешёвая проверка
// для гейта: 1 — можно поднимать AMF-энкодер, 0 — фолбэк на MF.
int katana_amf_available(void);

// katana_amf_create поднимает AMF AVC-энкодер на переданном D3D11-устройстве (том же,
// что WGC-захват). High profile + слайсы + intra-refresh + CBR. out_hr/out_stage — для
// диагностики (стадии: 1 dll, 2 AMFInit, 3 CreateContext, 4 InitDX11, 5 CreateComponent,
// 6 configure/Init). out_info заполняется краткой сводкой ("AMF ... slices=N ir=M").
katana_amf *katana_amf_create(void *d3d_device, int width, int height, int fps,
                              int bitrate_kbps, int gop, int32_t *out_hr, int *out_stage,
                              char *out_info, int info_cap);

// katana_amf_extradata кладёт SPS/PPS (Annex-B) в buf — для прайминга Go-кеша заголовков
// (AMF по умолчанию не вставляет их инлайн в кадры). Возвращает число байт (0 — нет).
int katana_amf_extradata(katana_amf *e, uint8_t *buf, int cap);

// katana_amf_submit кладёт один NV12-кадр (системная память) — байтовый фолбэк-путь.
int katana_amf_submit(katana_amf *e, const uint8_t *nv12, int len);

// katana_amf_poll забирает один готовый Annex-B access unit. >0 — байт, 0 — пока нет,
// <0 — ошибка/переполнение буфера.
int katana_amf_poll(katana_amf *e, uint8_t *buf, int buflen);

// zero-copy путь (как в MF): init_vproc → capture_texture → encode_captured.
int katana_amf_init_vproc(katana_amf *e, int src_w, int src_h, int32_t *out_hr);
int katana_amf_capture_texture(katana_amf *e, void *bgra_tex);
int katana_amf_encode_captured(katana_amf *e);

// katana_amf_force_keyframe просит IDR на следующем кадре (ответ на PLI зрителя).
void katana_amf_force_keyframe(katana_amf *e);

// katana_amf_set_bitrate меняет целевой битрейт (kbps) на лету.
void katana_amf_set_bitrate(katana_amf *e, int kbps);

// katana_amf_destroy останавливает и освобождает энкодер.
void katana_amf_destroy(katana_amf *e);

#endif // KATANA_AMF_ENCODER_H
