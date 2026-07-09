//go:build windows && arm64

package capture

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// callMFTEnumEx для arm64: по AAPCS64 16-байтная структура (GUID категории) при
// передаче по значению кладётся в два последовательных регистра (X0,X1), поэтому
// раскладываем её на lo/hi. На x64 раскладка иная (см. amd64-вариант — указатель).
func callMFTEnumEx(cat *windows.GUID, flags, inType, outType, ppActivate, pCount uintptr) uintptr {
	pcat := unsafe.Pointer(cat)
	lo := *(*uint64)(pcat)
	hi := *(*uint64)(unsafe.Pointer(uintptr(pcat) + 8))
	hr, _, _ := procMFTEnumEx.Call(uintptr(lo), uintptr(hi), flags, inType, outType, ppActivate, pCount)
	runtime.KeepAlive(cat)
	return hr
}
