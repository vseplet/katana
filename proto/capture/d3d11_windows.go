//go:build windows

package capture

// D3D11-часть Windows-захвата: создание устройства (нужно WGC как источник кадров),
// мост ID3D11Device → WinRT IDirect3DDevice (CreateDirect3D11DeviceFromDXGIDevice),
// и вытаскивание BGRA-пикселей из кадрового ID3D11Texture2D через CPU-staging копию.

import (
	"log"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	d3d11                        = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice        = d3d11.NewProc("D3D11CreateDevice")
	procCreateDirect3D11FromDXGI = d3d11.NewProc("CreateDirect3D11DeviceFromDXGIDevice")
)

const (
	driverHardware = 1 // D3D_DRIVER_TYPE_HARDWARE
	driverWARP     = 5 // D3D_DRIVER_TYPE_WARP (софтовый растеризатор — для VM без GPU)

	d3d11CreateDeviceBGRASupport = 0x20 // требуется для интеропа с WGC/Direct2D
	d3d11SDKVersion              = 7

	dxgiFormatB8G8R8A8Unorm = 87 // DXGI_FORMAT_B8G8R8A8_UNORM (BGRA) — формат кадров WGC

	d3d11UsageStaging  = 3
	d3d11CPUAccessRead = 0x20000
	d3d11MapRead       = 1

	// Индексы vtable
	idxDeviceCreateTexture2D = 5  // ID3D11Device::CreateTexture2D
	idxCtxMap                = 14 // ID3D11DeviceContext::Map
	idxCtxUnmap              = 15 // ID3D11DeviceContext::Unmap
	idxCtxCopyResource       = 47 // ID3D11DeviceContext::CopyResource
	idxTex2DGetDesc          = 10 // ID3D11Texture2D::GetDesc
	idxDxgiIfaceAccessGet    = 3  // IDirect3DDxgiInterfaceAccess::GetInterface
	idxMultithreadSetProt    = 5  // ID3D11Multithread::SetMultithreadProtected
)

// enableMultithreadProtection включает потокобезопасность immediate context'а.
// КРИТИЧНО: free-threaded framepool WGC рендерит кадры в свои текстуры на своём
// внутреннем потоке через ТОТ ЖЕ девайс/контекст, что и наш CopyResource+Map.
// Immediate context один на девайс и не потокобезопасен → без этого при потоке
// кадров (перетаскивание окна) два потока рвут контекст и мы читаем полукадры.
func enableMultithreadProtection(ctx uintptr) {
	mt, err := comQueryInterface(ctx, &iidID3D11Multithread)
	if err != nil {
		return
	}
	comCall(mt, idxMultithreadSetProt, 1) // TRUE
	comRelease(mt)
}

// d3d11Texture2DDesc — раскладка D3D11_TEXTURE2D_DESC (11×uint32 = 44 байта).
type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

// d3d11MappedSubresource — D3D11_MAPPED_SUBRESOURCE (указатель + 2 uint32).
type d3d11MappedSubresource struct {
	pData      uintptr
	RowPitch   uint32
	DepthPitch uint32
}

// createD3D11Device создаёт ID3D11Device (+immediate context). Пробуем аппаратный
// драйвер, при неудаче (VM без GPU) — WARP (софтовый).
func createD3D11Device() (dev, ctx uintptr, err error) {
	try := func(driver uintptr) uintptr {
		pd, pc := new(uintptr), new(uintptr) // out-слоты в куче
		hr, _, _ := procD3D11CreateDevice.Call(
			0,      // pAdapter
			driver, // DriverType
			0,      // Software
			d3d11CreateDeviceBGRASupport,
			0, 0, // pFeatureLevels, FeatureLevels
			d3d11SDKVersion,
			uintptr(unsafe.Pointer(pd)),
			0, // pFeatureLevel
			uintptr(unsafe.Pointer(pc)),
		)
		if hr == sOK {
			dev, ctx = *pd, *pc
		}
		runtime.KeepAlive(pd)
		runtime.KeepAlive(pc)
		return hr
	}
	if hr := try(driverHardware); hr == sOK {
		enableMultithreadProtection(ctx)
		log.Printf("capture: D3D11 device = HARDWARE (аппаратный GPU), MT-protection ON")
		return dev, ctx, nil
	}
	if hr := try(driverWARP); hr != sOK {
		return 0, 0, hrError(hr, "D3D11CreateDevice")
	}
	enableMultithreadProtection(ctx)
	log.Printf("capture: D3D11 device = WARP (софтовый растеризатор), MT-protection ON")
	return dev, ctx, nil
}

// d3dDeviceToWinRT приводит ID3D11Device к WinRT IDirect3DDevice (нужен WGC для
// создания framepool).
func d3dDeviceToWinRT(dev uintptr) (uintptr, error) {
	dxgiDev, err := comQueryInterface(dev, &iidIDXGIDevice)
	if err != nil {
		return 0, err
	}
	defer comRelease(dxgiDev)

	hr, inspectable := procCallOut(procCreateDirect3D11FromDXGI, dxgiDev)
	if err := hrError(hr, "CreateDirect3D11DeviceFromDXGIDevice"); err != nil {
		return 0, err
	}
	defer comRelease(inspectable)

	winrtDev, err := comQueryInterface(inspectable, &iidIDirect3DDevice)
	if err != nil {
		return 0, err
	}
	return winrtDev, nil
}

// textureDesc читает D3D11_TEXTURE2D_DESC текстуры.
func textureDesc(tex uintptr) d3d11Texture2DDesc {
	d := new(d3d11Texture2DDesc)
	comCall(tex, idxTex2DGetDesc, uintptr(unsafe.Pointer(d)))
	runtime.KeepAlive(d)
	return *d
}

// surfaceToTexture извлекает ID3D11Texture2D из WinRT IDirect3DSurface кадра.
func surfaceToTexture(surface uintptr) (uintptr, error) {
	access, err := comQueryInterface(surface, &iidIDirect3DDxgiInterfaceAccess)
	if err != nil {
		return 0, err
	}
	defer comRelease(access)
	hr, tex := comCallOut(access, idxDxgiIfaceAccessGet,
		uintptr(unsafe.Pointer(&iidID3D11Texture2D)))
	if err := hrError(hr, "GetInterface(ID3D11Texture2D)"); err != nil {
		return 0, err
	}
	return tex, nil
}

// createStagingTexture создаёт CPU-читаемую staging-текстуру заданного размера в
// формате BGRA (для копии кадра из GPU и последующего Map).
func createStagingTexture(dev uintptr, w, h uint32) (uintptr, error) {
	desc := new(d3d11Texture2DDesc) // на куче — адрес стабилен на время нативного вызова
	*desc = d3d11Texture2DDesc{
		Width:          w,
		Height:         h,
		MipLevels:      1,
		ArraySize:      1,
		Format:         dxgiFormatB8G8R8A8Unorm,
		SampleCount:    1,
		SampleQuality:  0,
		Usage:          d3d11UsageStaging,
		BindFlags:      0,
		CPUAccessFlags: d3d11CPUAccessRead,
		MiscFlags:      0,
	}
	hr, tex := comCallOut(dev, idxDeviceCreateTexture2D,
		uintptr(unsafe.Pointer(desc)), 0)
	runtime.KeepAlive(desc)
	if err := hrError(hr, "CreateTexture2D(staging)"); err != nil {
		return 0, err
	}
	return tex, nil
}

// mapStaging копирует src-текстуру в staging и мапит её на CPU. Возвращает
// указатель на пиксели, rowPitch (байт на строку) и функцию Unmap. Вызывающий
// обязан вызвать unmap после чтения.
func mapStaging(ctx, staging, src uintptr) (data uintptr, rowPitch int, unmap func(), err error) {
	comCall(ctx, idxCtxCopyResource, staging, src)
	m := new(d3d11MappedSubresource) // out-структура на куче
	hr := comCall(ctx, idxCtxMap, staging, 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(m)))
	runtime.KeepAlive(m)
	if e := hrError(hr, "Map(staging)"); e != nil {
		return 0, 0, func() {}, e
	}
	return m.pData, int(m.RowPitch), func() { comCall(ctx, idxCtxUnmap, staging, 0) }, nil
}
