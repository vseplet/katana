#ifndef KATANA_MF_ENCODER_H
#define KATANA_MF_ENCODER_H

#include <stdint.h>

// Нативный аппаратный H264-энкодер поверх Media Foundation (async hardware MFT).
// D3D11-устройство передаётся снаружи (то же, на котором WGC отдаёт кадры экрана) —
// энкодер вешается на него через IMFDXGIDeviceManager, поэтому захват и кодирование
// делят один GPU-контекст, без второго девайса и без конфликта (как у OBS/Sunshine).
//
// Модель работы асинхронная: MFT сам просит вход (METransformNeedInput) и отдаёт
// выход (METransformHaveOutput) через IMFAsyncCallback. Go-сторона просто кладёт
// NV12-кадры (katana_enc_submit) и забирает готовые Annex-B access unit'ы
// (katana_enc_poll) — вся COM-асинхронщина спрятана в C.

typedef struct katana_enc katana_enc;

// katana_enc_create поднимает аппаратный H264-MFT на переданном D3D11-устройстве.
//   d3d_device   — ID3D11Device* (uintptr из Go), на котором работает WGC-захват.
//   width/height — размер кадра (чётные, NV12).
//   fps          — целевая частота.
//   bitrate_kbps — целевой битрейт (CBR).
//   gop          — интервал ключевых кадров в кадрах (0 = по умолчанию энкодера).
// Возвращает хендл или NULL; в *out_hr кладёт HRESULT для диагностики.
// out_stage (может быть NULL) получает номер провалившегося шага для диагностики:
//   1 MFStartup, 2 MFTEnumEx, 3 нет аппаратного энкодера, 4 ActivateObject,
//   5 SetOutputType, 6 SetInputType, 7 QI EventGenerator, 8 BeginGetEvent.
katana_enc *katana_enc_create(void *d3d_device, int width, int height, int fps,
                              int bitrate_kbps, int gop, int32_t *out_hr, int *out_stage);

// katana_enc_submit кладёт один NV12-кадр (системная память, длина width*height*3/2).
// Неблокирующе: кадр ставится в очередь, реально скармливается на METransformNeedInput.
// 0 — ок, <0 — ошибка (энкодер мёртв).
int katana_enc_submit(katana_enc *e, const uint8_t *nv12, int len);

// katana_enc_poll забирает один готовый access unit (Annex-B) в buf (ёмкость buflen).
// Возвращает число записанных байт, 0 — пока нечего, <0 — ошибка/переполнение буфера.
int katana_enc_poll(katana_enc *e, uint8_t *buf, int buflen);

// katana_enc_destroy останавливает и освобождает энкодер.
void katana_enc_destroy(katana_enc *e);

#endif // KATANA_MF_ENCODER_H
