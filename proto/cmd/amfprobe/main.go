//go:build windows && cgo && winnative

// amfprobe — автономный диагностический бинарь: проверяет, работает ли AMD AMF SDK
// из нашего mingw-сборки и принимает ли AMD слайсы + intra-refresh (то, что Media
// Foundation молча игнорил). НЕ связан с основным стримом — можно гонять отдельно,
// пока идёт трансляция. Отчёт печатается в stdout и дублируется в ~/.katana/amfprobe.log.
package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../thirdparty/amf -D_WIN32_WINNT=0x0A00
#include <stdlib.h>
#include "amf_probe.h"
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

func main() {
	buf := make([]byte, 8192)
	n := int(C.amf_probe((*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	report := string(buf[:n])
	fmt.Print(report)

	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".katana", "amfprobe.log")
		if os.MkdirAll(filepath.Dir(p), 0o755) == nil && os.WriteFile(p, []byte(report), 0o644) == nil {
			fmt.Printf("\n[amfprobe] отчёт записан в %s\n", p)
		}
	}
}
