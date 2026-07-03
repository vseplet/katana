#!/bin/sh
# Локальный запуск хоста katana на Steam Deck прямо из исходников (без релиза и
# тегов): собирает proto/ и стартует с тестовой сессией. Требует Go в PATH.
#
#   git pull && sh steamos.sh
#
set -e

SESSION="5ca6efe2-3e5d-43f3-b92b-c3966a196fec"
BROKER="wss://katana.vseplet.deno.net/rtc"

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR/proto"

command -v go >/dev/null 2>&1 || { echo "error: go не найден в PATH — нужен для сборки"; exit 1; }
command -v ffmpeg >/dev/null 2>&1 || echo "warn: ffmpeg не найден — не будет ни видео, ни звука"

WAYLAND=0
case "${XDG_SESSION_TYPE}${WAYLAND_DISPLAY}" in *wayland*) WAYLAND=1;; esac

# --- Видео на Wayland идёт через xdg-desktop-portal (ScreenCast) + PipeWire,
# кодируется GStreamer'ом (pipewiresrc → x264enc). Проверим наличие gst и плагинов.
# Портал при первом запуске покажет диалог KDE «разрешить захват экрана». ---
if [ "$WAYLAND" = 1 ]; then
  echo "note: сессия Wayland — видео через портал ScreenCast + GStreamer (будет диалог «разрешить захват»)"
  if ! command -v gst-launch-1.0 >/dev/null 2>&1; then
    echo "warn: gst-launch-1.0 не найден — видео на Wayland не пойдёт (звук/ввод/терминал будут)."
  else
    for el in pipewiresrc x264enc h264parse; do
      gst-inspect-1.0 "$el" >/dev/null 2>&1 || \
        echo "warn: нет gst-элемента '$el' — видео не пойдёт. Нужны gstreamer плагины (pipewire + x264)."
    done
  fi
fi

# --- Ввод: uinput, работает и в X11, и в Wayland ---
if [ ! -w /dev/uinput ]; then
  echo "warn: нет доступа на запись в /dev/uinput — ВВОД не заработает. Разово (sudo):"
  echo "        sudo modprobe uinput && sudo chgrp \"$(id -gn)\" /dev/uinput && sudo chmod 660 /dev/uinput"
fi

echo "build…"
go build -o ../katana-host .
echo "run (session $SESSION, broker $BROKER)…"
exec ../katana-host --session "$SESSION" --broker "$BROKER" --audio
