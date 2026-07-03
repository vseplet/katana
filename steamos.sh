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

# --- Видео на Wayland: kmsgrab снимает экран с DRM-сканаута, но требует прав
# CAP_SYS_ADMIN у ffmpeg. /usr/bin/ffmpeg на read-only rootfs патчить нельзя,
# поэтому кладём копию в ~/.katana/bin/ffmpeg (её хост берёт приоритетно) и вешаем
# capability на копию. Разово, нужен sudo. ---
if [ "$WAYLAND" = 1 ]; then
  echo "note: сессия Wayland — видео через kmsgrab (нужны права ffmpeg на DRM)"
  KBIN="$HOME/.katana/bin"; KFF="$KBIN/ffmpeg"
  mkdir -p "$KBIN"
  SYS_FF="$(command -v ffmpeg || echo /usr/bin/ffmpeg)"
  if [ ! -x "$KFF" ] || [ "$SYS_FF" -nt "$KFF" ]; then
    cp -f "$SYS_FF" "$KFF" 2>/dev/null || echo "warn: не смог скопировать ffmpeg в $KFF"
  fi
  if ! getcap "$KFF" 2>/dev/null | grep -q cap_sys_admin; then
    echo "note: выдаю ffmpeg права на DRM (sudo setcap) — иначе kmsgrab не снимет экран"
    sudo setcap cap_sys_admin+ep "$KFF" 2>/dev/null \
      || echo "warn: setcap не удался — видео на Wayland не пойдёт (звук/ввод/терминал будут). Выполни вручную: sudo setcap cap_sys_admin+ep \"$KFF\""
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
