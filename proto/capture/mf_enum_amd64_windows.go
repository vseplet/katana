//go:build windows && amd64

package capture

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// callMFTEnumEx вызывает MFTEnumEx с учётом ABI x64. Первый аргумент MFTEnumEx —
// GUID категории ПО ЗНАЧЕНИЮ (16 байт). По x64-соглашению агрегат размером не
// {1,2,4,8} байт передаётся УКАЗАТЕЛЕМ (один регистр), а не двумя регистрами.
// Раньше GUID слался как (lo,hi) — это верно для arm64 (там 16 байт идут в X0,X1),
// но на x64 сдвигало все последующие аргументы на регистр: в слот Flags попадала
// верхняя половина GUID → MFTEnumEx возвращал ERROR_INVALID_FLAGS (0x800703ec), и
// аппаратный H264-энкодер НИКОГДА не находился (везде откат на софтовый).
func callMFTEnumEx(cat *windows.GUID, flags, inType, outType, ppActivate, pCount uintptr) uintptr {
	hr, _, _ := procMFTEnumEx.Call(uintptr(unsafe.Pointer(cat)), flags, inType, outType, ppActivate, pCount)
	runtime.KeepAlive(cat)
	return hr
}
