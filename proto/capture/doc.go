//go:build linux

package capture

// Матрица поддержки Linux-захвата (единый источник правды).
//
// ВАЖНО: композитор (KDE/GNOME/…) и вендор GPU (AMD/Intel/NVIDIA) — это
// RUNTIME-свойства, их build-тегом Go не выразить (теги знают только про
// linux/cgo/арх). Поэтому зависимость от KDE и AMD фиксируется тремя способами:
// (1) этой матрицей, (2) честными именами файлов backend_*, (3) runtime-детектом
// и логом выбранного бэкенда в linux_common.go (см. describeEnv/videoBackend).
//
//	Ось          Поддержано (tuned)      Вероятно      Не поддержано
//	----------   ---------------------   -----------   -------------------
//	session      Wayland                 X11 (fallback)
//	композитор   KDE/KWin                GNOME         wlroots (нет ввода)
//	портал       RemoteDesktop+ScreenCast(KDE/GNOME)   wlroots: только ScreenCast
//	энкод        VAAPI h264 (AMD)        Intel VAAPI   NVIDIA/NVENC
//	dmabuf       AMD (фильтр DCC)        Intel         прочее
//	audio        PulseAudio monitor
//	input        портал (Wayland) / uinput (X11)
//
// НЕ поддержано: инъекция ввода без RemoteDesktop-портала (wlroots/Sway),
// NVENC-энкод, headless/консоль (нет захвата фреймбуфера/TTY), SteamOS Game Mode
// (gamescope — отдельная сессия, хост живёт в Plasma и умирает при переключении).
//
// Реальный целевой профиль сборки: Steam Deck / SteamOS, Desktop Mode (KDE
// Plasma Wayland), AMD.
//
// Расширение охвата — новые файлы backend_* как соседи (напр.
// backend_gamescope_uinput, backend_nvenc, backend_headless_cage), реализующие
// тот же контур, плюс строка в этой матрице и ветка в videoBackend().
//
// Раскладка файлов:
//
//	linux_common.go                — детект сессии/окружения, выбор бэкенда, Stream-обвязка, ScreenSize
//	audio_pulse_linux.go           — звук: PulseAudio monitor → Opus (ffmpeg)
//	backend_portal_vaapi_linux.*   — Wayland: портал+PipeWire → VAAPI (dmabuf zero-copy, cgo)
//	backend_wayland_gst_linux.go   — Wayland fallback: портал → gst → ffmpeg (CPU-пайп)
//	backend_x11grab_linux.go       — X11: x11grab → ffmpeg
//	portal_linux.go                — xdg-desktop-portal (RemoteDesktop+ScreenCast), generic KDE/GNOME
