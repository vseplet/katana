//go:build windows

package main

// Windows-псевдотерминал для общего терминала (terminal.go) через ConPTY
// (CreatePseudoConsole, Windows 10 1809+). Чистый Go поверх x/sys/windows, без cgo:
// создаём два пайпа (наш stdin → conpty, вывод conpty → нам), псевдоконсоль, затем
// cmd.exe с атрибутом PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE в STARTUPINFOEX. Размер
// меняем ResizePseudoConsole. creack/pty на Windows не работает — там pty недоступен.

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32DLL             = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole = kernel32DLL.NewProc("CreatePseudoConsole")
	procResizePseudoConsole = kernel32DLL.NewProc("ResizePseudoConsole")
	procClosePseudoConsole  = kernel32DLL.NewProc("ClosePseudoConsole")
)

// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE — привязывает создаваемый процесс к ConPTY.
const procThreadAttrPseudoConsole = 0x00020016

// coordDword упаковывает размер (COORD: X=cols в младшем слове, Y=rows в старшем)
// в DWORD — так COORD передаётся по значению в Create/ResizePseudoConsole.
func coordDword(cols, rows uint16) uintptr {
	return uintptr(uint32(cols) | uint32(rows)<<16)
}

type winPTY struct {
	hpc     windows.Handle // HPCON
	in      *os.File       // пишем сюда stdin шелла
	out     *os.File       // читаем отсюда вывод шелла
	proc    windows.Handle // хендл процесса шелла (для завершения)
	attrs   *windows.ProcThreadAttributeListContainer
	cmdline []uint16 // держим живым на время CreateProcess
}

// startPTY поднимает cmd.exe в ConPTY заданного размера.
func startPTY(cols, rows uint16) (ptySession, error) {
	// Пайпы: conpty читает stdin из inRead, пишет вывод в outWrite; у себя держим
	// противоположные концы (inWrite/outRead).
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe(in): %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("CreatePipe(out): %w", err)
	}

	var hpc windows.Handle
	r, _, _ := procCreatePseudoConsole.Call(
		coordDword(cols, rows), uintptr(inRead), uintptr(outWrite), 0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	// conpty дублирует переданные концы — свои копии inRead/outWrite закрываем в
	// любом случае (при успехе и при ошибке).
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	if r != 0 { // HRESULT != S_OK
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("CreatePseudoConsole: hr=0x%08x", uint32(r))
	}

	// STARTUPINFOEX с атрибутом псевдоконсоли.
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		closeConPTY(hpc, inWrite, outRead)
		return nil, fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	if err := attrs.Update(procThreadAttrPseudoConsole, unsafe.Pointer(hpc), unsafe.Sizeof(hpc)); err != nil {
		attrs.Delete()
		closeConPTY(hpc, inWrite, outRead)
		return nil, fmt.Errorf("UpdateProcThreadAttribute(pseudoconsole): %w", err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrs.List()

	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmdline, err := windows.UTF16FromString(shell)
	if err != nil {
		attrs.Delete()
		closeConPTY(hpc, inWrite, outRead)
		return nil, err
	}
	var cwd *uint16
	if home, herr := os.UserHomeDir(); herr == nil {
		cwd, _ = windows.UTF16PtrFromString(home)
	}

	var pi windows.ProcessInformation
	// inheritHandles=false: дочерний процесс получает консоль через атрибут
	// псевдоконсоли, а не через наследование хендлов.
	err = windows.CreateProcess(nil, &cmdline[0], nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT, nil, cwd, &si.StartupInfo, &pi)
	if err != nil {
		attrs.Delete()
		closeConPTY(hpc, inWrite, outRead)
		return nil, fmt.Errorf("CreateProcess(%s): %w", shell, err)
	}
	windows.CloseHandle(pi.Thread) // поток не нужен, процесс держим для kill

	return &winPTY{
		hpc:     hpc,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
		proc:    pi.Process,
		attrs:   attrs,
		cmdline: cmdline,
	}, nil
}

func (w *winPTY) Read(p []byte) (int, error)  { return w.out.Read(p) }
func (w *winPTY) Write(p []byte) (int, error) { return w.in.Write(p) }

func (w *winPTY) Resize(cols, rows uint16) error {
	r, _, _ := procResizePseudoConsole.Call(uintptr(w.hpc), coordDword(cols, rows))
	if r != 0 {
		return fmt.Errorf("ResizePseudoConsole: hr=0x%08x", uint32(r))
	}
	return nil
}

func (w *winPTY) Close() error {
	if w.proc != 0 {
		_ = windows.TerminateProcess(w.proc, 0)
		windows.CloseHandle(w.proc)
		w.proc = 0
	}
	// ClosePseudoConsole после TerminateProcess (иначе может ждать вычитки вывода).
	if w.hpc != 0 {
		procClosePseudoConsole.Call(uintptr(w.hpc))
		w.hpc = 0
	}
	if w.out != nil {
		_ = w.out.Close()
	}
	if w.in != nil {
		_ = w.in.Close()
	}
	if w.attrs != nil {
		w.attrs.Delete()
		w.attrs = nil
	}
	return nil
}

// closeConPTY — аварийная уборка при ошибке инициализации до создания winPTY.
func closeConPTY(hpc, inWrite, outRead windows.Handle) {
	procClosePseudoConsole.Call(uintptr(hpc))
	windows.CloseHandle(inWrite)
	windows.CloseHandle(outRead)
}
