//go:build windows && cgo && winnative

#ifndef KATANA_AMF_PROBE_H
#define KATANA_AMF_PROBE_H

// amf_probe гоняет автономную проверку AMD AMF SDK и пишет многострочный текстовый
// отчёт в out (не более cap байт), возвращает длину. Сам ничего в файлы не пишет и
// НИКАК не связан с основным стримом — это отдельный диагностический бинарь.
//
// Что проверяет:
//   1) грузится ли amfrt64.dll (рантайм в драйвере AMD) из нашего mingw-бинаря;
//   2) заводится ли AMF factory/context/DX11/энкодер (ABI mingw<->MSVC-DLL);
//   3) ПРИНИМАЕТ ли AMD слайсы и intra-refresh (readback GetProperty) — то, что
//      Media Foundation молча игнорил (1.0 slices/frame, ir=IGNORED);
//   4) выдаёт ли валидный H264 и сколько РЕАЛЬНО слайсов в битстриме.
int amf_probe(char *out, int cap);

#endif
