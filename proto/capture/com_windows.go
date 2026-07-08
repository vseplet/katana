//go:build windows

package capture

// Минимальный COM/WinRT интероп на чистом Go (без cgo): загрузка DLL через
// LazyDLL, вызов методов через vtable (syscall.SyscallN по указателю метода),
// хелперы IUnknown/IInspectable, WinRT-строки (HSTRING) и активация классов.
// Раскладка vtable: IUnknown = {QueryInterface:0, AddRef:1, Release:2};
// IInspectable добавляет {GetIids:3, GetRuntimeClassName:4, GetTrustLevel:5},
// поэтому методы WinRT-интерфейсов начинаются с индекса 6; классические
// COM-интерфейсы (интеропы) — с индекса 3.

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const ptrSize = unsafe.Sizeof(uintptr(0))

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procIsBadReadPtr  = kernel32.NewProc("IsBadReadPtr")

	combase                    = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize           = combase.NewProc("RoInitialize")
	procRoUninitialize         = combase.NewProc("RoUninitialize")
	procRoGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateString    = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString    = combase.NewProc("WindowsDeleteString")
)

const (
	roInitMultithreaded = 1 // RO_INIT_MULTITHREADED
	sOK                 = 0 // S_OK
)

// hrError оборачивает ненулевой HRESULT в error.
func hrError(hr uintptr, what string) error {
	if hr == sOK {
		return nil
	}
	return fmt.Errorf("%s: hresult 0x%08x", what, uint32(hr))
}

// comCall вызывает метод idx из vtable COM-объекта this (первым аргументом всегда
// сам this). Возвращает r1 (для методов с HRESULT — это HRESULT).
func comCall(this uintptr, idx int, args ...uintptr) uintptr {
	if this == 0 {
		return uintptr(0x80004003) // E_POINTER
	}
	vtbl := *(*uintptr)(unsafe.Pointer(this))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx)*ptrSize))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, this)
	all = append(all, args...)
	r1, _, _ := syscall.SyscallN(fn, all...)
	return r1
}

// КРИТИЧНО про out-параметры: Go двигает стеки горутин при их росте, а
// uintptr(unsafe.Pointer(&stackVar)) при переносе стека НЕ обновляется → нативный
// код пишет по протухшему адресу, мы читаем мусор → access violation. Поэтому под
// каждый out-указатель берём new() (куча — её объекты Go не двигает) и держим его
// живым KeepAlive поверх вызова. Входные указатели должны указывать на глобалы
// (стабильны) либо тоже на heap-объекты, живые на время вызова.

// comCallOut вызывает метод, у которого ПОСЛЕДНИЙ параметр — out **obj; кладёт
// out-слот в кучу и возвращает (hr, полученный указатель).
func comCallOut(this uintptr, idx int, args ...uintptr) (hr, out uintptr) {
	p := new(uintptr)
	full := make([]uintptr, len(args)+1)
	copy(full, args)
	full[len(args)] = uintptr(unsafe.Pointer(p))
	hr = comCall(this, idx, full...)
	out = *p
	runtime.KeepAlive(p)
	return
}

// procCallOut — то же для LazyProc-функции с трейлинг out **obj.
func procCallOut(proc *windows.LazyProc, args ...uintptr) (hr, out uintptr) {
	p := new(uintptr)
	full := make([]uintptr, len(args)+1)
	copy(full, args)
	full[len(args)] = uintptr(unsafe.Pointer(p))
	r, _, _ := proc.Call(full...)
	out = *p
	runtime.KeepAlive(p)
	return r, out
}

// ptrReadable — указывает ли p на читаемую (закоммиченную) память достаточного
// размера под vtable-указатель. Диагностический предохранитель: не даёт
// дереференсу битого COM-указателя уронить процесс access violation'ом.
func ptrReadable(p uintptr) bool {
	if p == 0 {
		return false
	}
	r, _, _ := procIsBadReadPtr.Call(p, ptrSize)
	return r == 0 // IsBadReadPtr != 0 → память НЕчитаема
}

// --- IUnknown ------------------------------------------------------------------

func comAddRef(this uintptr) uint32  { return uint32(comCall(this, 1)) }
func comRelease(this uintptr) uint32 {
	if this == 0 {
		return 0
	}
	return uint32(comCall(this, 2))
}

// comQueryInterface — QueryInterface(iid) → указатель на нужный интерфейс.
func comQueryInterface(this uintptr, iid *windows.GUID) (uintptr, error) {
	hr, out := comCallOut(this, 0, uintptr(unsafe.Pointer(iid)))
	runtime.KeepAlive(iid)
	if err := hrError(hr, "QueryInterface"); err != nil {
		return 0, err
	}
	return out, nil
}

// --- IClosable (WinRT): Close на индексе 6 -------------------------------------

func winrtClose(this uintptr) {
	if this == 0 {
		return
	}
	c, err := comQueryInterface(this, &iidIClosable)
	if err != nil {
		return
	}
	comCall(c, 6)
	comRelease(c)
}

// --- WinRT init / HSTRING / активация ------------------------------------------

// roInitialize поднимает WinRT (MTA) на текущем потоке. Идемпотентно на уровне
// апартамента; парный roUninitialize вызывается при завершении.
func roInitialize() error {
	hr, _, _ := procRoInitialize.Call(roInitMultithreaded)
	// S_FALSE (1) — уже инициализировано на этом потоке, это ок.
	if hr != sOK && hr != 1 {
		return hrError(hr, "RoInitialize")
	}
	return nil
}

func roUninitialize() { procRoUninitialize.Call() }

// newHString создаёт WinRT-строку из Go-строки.
func newHString(s string) (uintptr, error) {
	u16, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	hr, hs := procCallOut(procWindowsCreateString,
		uintptr(unsafe.Pointer(&u16[0])),
		uintptr(len(u16)-1), // без завершающего NUL
	)
	runtime.KeepAlive(u16)
	if err := hrError(hr, "WindowsCreateString"); err != nil {
		return 0, err
	}
	return hs, nil
}

func deleteHString(hs uintptr) {
	if hs != 0 {
		procWindowsDeleteString.Call(hs)
	}
}

// activationFactory возвращает фабрику активации WinRT-класса, приведённую к iid
// (может быть как IActivationFactory/статики, так и интероп-интерфейс).
func activationFactory(class string, iid *windows.GUID) (uintptr, error) {
	hs, err := newHString(class)
	if err != nil {
		return 0, err
	}
	defer deleteHString(hs)
	hr, f := procCallOut(procRoGetActivationFactory,
		hs, uintptr(unsafe.Pointer(iid)))
	runtime.KeepAlive(iid)
	if err := hrError(hr, "RoGetActivationFactory("+class+")"); err != nil {
		return 0, err
	}
	return f, nil
}

// --- GUID константы (IID интерфейсов и атрибутов) ------------------------------
// Парсим из канонической строки — так меньше шанс опечатки в полях структуры.

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic("capture: bad GUID literal " + s)
	}
	return g
}

var (
	// Windows.Graphics.Capture
	iidIGraphicsCaptureItem              = mustGUID("{79C3F95B-31F7-4EC2-A464-632EF5D30760}")
	iidIGraphicsCaptureItemInterop       = mustGUID("{3628E81B-3CAC-4C60-B7F4-23CE0E0C3356}")
	iidIDirect3D11CaptureFramePool       = mustGUID("{24EB6D22-1975-422E-82E7-780DBD8DDF24}")
	iidIDirect3D11CaptureFramePoolStat2  = mustGUID("{589B103F-6BBC-5DF5-A991-02E28B3B66D5}")
	iidIDirect3D11CaptureFrame           = mustGUID("{FA50C623-38DA-4B32-ACF3-FA9734AD800E}")
	iidIGraphicsCaptureSession           = mustGUID("{814E42A9-F70F-4AD7-939B-FDDCC6EB880D}")
	iidIDirect3DDevice                   = mustGUID("{A37624AB-8D5F-4650-9D3E-9EAE3D9BC670}")
	iidIClosable                         = mustGUID("{30D5A829-7FA4-4026-83BB-D75BAE4EA99E}")
	iidIDirect3DDxgiInterfaceAccess      = mustGUID("{A9B3D012-3DF2-4EE3-B8D1-8695F457D3C1}")

	// Direct3D / DXGI
	iidIDXGIDevice    = mustGUID("{54EC77FA-1377-44E6-8C32-88FD5F44C84C}")
	iidID3D11Texture2D = mustGUID("{6F15AAF2-D208-4E89-9AB4-489535D34F9C}")

	// Media Foundation
	iidIMFTransform = mustGUID("{BF94C121-5B05-4E6F-8000-BA598961414D}")
	iidICodecAPI    = mustGUID("{901DB4C7-31CE-41A2-85DC-8FA0BF41B8DA}")
	clsidH264Encoder = mustGUID("{6CA50344-051A-4DED-9779-A43305165E35}") // CLSID_CMSH264EncoderMFT
)

// Активационные строки WinRT-классов.
const (
	classGraphicsCaptureItem     = "Windows.Graphics.Capture.GraphicsCaptureItem"
	classDirect3D11CaptureFramePool = "Windows.Graphics.Capture.Direct3D11CaptureFramePool"
)
