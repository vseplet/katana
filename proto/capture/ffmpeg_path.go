package capture

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// errNoFFmpeg — ffmpeg не найден ни рядом, ни в PATH.
var errNoFFmpeg = errors.New("ffmpeg not found: place a binary at ~/.katana/bin/ffmpeg or install ffmpeg (e.g. brew install ffmpeg)")

var (
	ffmpegOnce sync.Once
	ffmpegBin  string
)

// FFmpegPath возвращает путь к ffmpeg по правилу: сначала ~/.katana/bin/ffmpeg
// (бинарник, положенный рядом установщиком), затем — из PATH (глобальный).
// Пусто, если не найден нигде. Результат кэшируется.
func FFmpegPath() string {
	ffmpegOnce.Do(func() { ffmpegBin = resolveFFmpeg() })
	return ffmpegBin
}

func resolveFFmpeg() string {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	// 1) рядом с хостом — ~/.katana/bin/ffmpeg (приоритет).
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".katana", "bin", name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	// 2) глобально — в PATH.
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return "" // 3) не найден
}
