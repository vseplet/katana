//go:build windows && cgo && winnative

// Нативный in-process аппаратный H264-энкодер на Windows: захваченная D3D11-текстура
// WGC отдаётся Media Foundation H264 MFT на ТОМ ЖЕ D3D11-устройстве (общий
// IMFDXGIDeviceManager), без копирования на CPU и без второго GPU-контекста — как
// делают OBS/Sunshine. Собирается только под тегом winnative (cgo на виндовом
// раннере), обычный релиз (CGO_ENABLED=0) продолжает жить на ffmpeg-подпроцессе.
//
// Это первый шаг: подтверждаем, что тулчейн (mingw) собирает cgo с заголовками
// Media Foundation / D3D11 и линкует системные либы. Реальный конвейер строится
// поверх этого в следующих итерациях.

package capture

/*
#cgo CFLAGS: -D_WIN32_WINNT=0x0A00 -DCOBJMACROS
#cgo LDFLAGS: -lmfplat -lmfuuid -lmf -ld3d11 -ldxgi -lole32 -luuid

#include <windows.h>
#include <d3d11.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <codecapi.h>

// katana_mf_startup — smoke: доступны ли символы Media Foundation в линковке.
static HRESULT katana_mf_startup(void) {
    HRESULT hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
    if (SUCCEEDED(hr)) {
        MFShutdown();
    }
    return hr;
}
*/
import "C"

// nativeMFStartup проверяет, что связка cgo↔Media Foundation работает в рантайме
// (используется в первичном smoke, дальше заменится реальной инициализацией энкодера).
func nativeMFStartup() int32 {
	return int32(C.katana_mf_startup())
}
