//go:build linux

package capture

// Видео на X11-десктопе: ffmpeg -f x11grab снимает root целиком, кодирует в
// H264 (libx264) или VP8 (libvpx) и отдаёт в stdout. Только для настоящего X11
// (не Xwayland) — на Wayland используется портал (video_wayland_linux.go).

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// x11Display — адрес входа x11grab ($DISPLAY, напр. ":0").
func x11Display() string {
	d := os.Getenv("DISPLAY")
	if d == "" {
		d = ":0"
	}
	return d
}

// buildX11Args — аргументы ffmpeg для x11grab с выходом H264/VP8 в stdout.
func buildX11Args(opts Options) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	if opts.TestSource {
		w := opts.Width
		if w <= 0 {
			w = 1280
		}
		args = append(args, "-re", "-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%dx720:rate=%d", w, opts.FPS))
	} else {
		draw := "0"
		if opts.Cursor {
			draw = "1"
		}
		args = append(args,
			"-f", "x11grab",
			"-framerate", fmt.Sprintf("%d", opts.FPS),
			"-draw_mouse", draw,
			"-i", x11Display())
	}

	args = append(args, "-r", fmt.Sprintf("%d", opts.FPS), "-fps_mode", "cfr")
	if opts.Width > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:-2", opts.Width))
	}
	args = append(args, "-pix_fmt", "yuv420p")

	if opts.Codec == CodecH264 {
		args = append(args,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "high", "-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			"-bsf:v", "dump_extra=freq=keyframe",
			"-f", "h264", "pipe:1")
	} else {
		args = append(args,
			"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8",
			"-lag-in-frames", "0", "-threads", fmt.Sprintf("%d", opts.Threads),
			"-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			"-keyint_min", fmt.Sprintf("%d", opts.FPS),
			"-f", "ivf", "pipe:1")
	}
	return args
}

// startVideoX11 запускает x11grab-ffmpeg и возвращает канал кадров.
func startVideoX11(ctx context.Context, opts Options) (chan []byte, error) {
	args := buildX11Args(opts)
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	log.Printf("capture: ffmpeg video (x11) %s", strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer waitKill(cmd, "video")
		in := bufio.NewReader(stdout)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()
	return frames, nil
}
