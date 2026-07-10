//go:build windows

package capture

import (
	"log"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows Job Object с KILL_ON_JOB_CLOSE: все назначенные процессы (наши ffmpeg)
// умирают, как только закрывается последний хэндл job'а — то есть при смерти katana,
// ДАЖЕ жёсткой (закрыли терминал, taskkill, краш). Без этого дочерний ffmpeg на
// Windows переживает родителя, продолжает держать AMF-сессию AMD, и они копятся,
// пока h264_amf не перестаёт подниматься совсем. Хэндл держим открытым весь процесс.
var (
	jobOnce   sync.Once
	jobHandle windows.Handle
)

func killJob() windows.Handle {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			log.Printf("capture: job object: create: %v", err)
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(
			h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			log.Printf("capture: job object: set info: %v", err)
			windows.CloseHandle(h)
			return
		}
		jobHandle = h
	})
	return jobHandle
}

// assignToKillJob привязывает уже запущенный процесс к kill-on-close job'у. Ошибки
// не фатальны — просто теряем гарантию авто-убийства для этого процесса.
func assignToKillJob(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job := killJob()
	if job == 0 {
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		log.Printf("capture: job object: open child %d: %v", cmd.Process.Pid, err)
		return
	}
	defer windows.CloseHandle(ph)
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		log.Printf("capture: job object: assign child %d: %v", cmd.Process.Pid, err)
	}
}
