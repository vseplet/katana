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

# --- Проверки окружения (не блокирующие) ---

command -v go >/dev/null 2>&1 || { echo "error: go не найден в PATH — нужен для сборки"; exit 1; }

# ffmpeg — для захвата экрана (x11grab) и звука (pulse). На Wayland видео сейчас
# отключается автоматически (см. VideoAvailable), останется звук + терминал.
command -v ffmpeg >/dev/null 2>&1 || \
  echo "warn: ffmpeg не найден — не будет видео/звука (терминал работает)"

case "${XDG_SESSION_TYPE}${WAYLAND_DISPLAY}" in
  *wayland*) echo "note: сессия Wayland — захват экрана через x11grab недоступен (будет только звук + терминал + ввод)";;
esac

# uinput — для ввода (мышь/клава), работает и в X11, и в Wayland.
if [ ! -w /dev/uinput ]; then
  echo "warn: нет доступа на запись в /dev/uinput — ВВОД не заработает."
  echo "      разово включить (нужен sudo):"
  echo "        sudo modprobe uinput"
  echo "        sudo chgrp \"$(id -gn)\" /dev/uinput && sudo chmod 660 /dev/uinput"
fi

# --- Сборка и запуск ---
echo "build…"
go build -o ../katana-host .
echo "run (session $SESSION, broker $BROKER)…"
exec ../katana-host --session "$SESSION" --broker "$BROKER" --audio
