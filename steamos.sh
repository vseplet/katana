#!/bin/sh
# Запуск нативного хоста katana на Steam Deck: качает готовый бинарь из CI и
# стартует с тестовой сессией. Go/сборка на Deck больше не нужны.
#
#   sh steamos.sh
#
# Бинарь собирается воркфлоу native-linux (Actions → Run workflow) в Arch-
# контейнере (совпадает со SteamOS) и публикуется в пре-релиз с тегом native.
set -e

SESSION="5ca6efe2-3e5d-43f3-b92b-c3966a196fec"
BROKER="wss://katana.vseplet.deno.net/rtc"

DIR="$HOME/.katana/bin"
BIN="$DIR/katana-native"
URL="https://github.com/vseplet/katana/releases/download/native/katana-linux-amd64-native"
mkdir -p "$DIR"

echo "downloading native build…"
curl -fL "$URL" -o "$BIN" || { echo "error: не скачался $URL — сначала запусти workflow native-linux (Actions → Run workflow)"; exit 1; }
chmod +x "$BIN"

# --- Проверки рантайма (не блокирующие) ---
# Видео на Wayland сейчас идёт через gst→ffmpeg-фолбэк (нативный GPU-путь в
# разработке), звук — через ffmpeg, поэтому нужны ffmpeg и gst.
command -v ffmpeg >/dev/null 2>&1 || echo "warn: ffmpeg не найден — не будет ни видео, ни звука"

case "${XDG_SESSION_TYPE}${WAYLAND_DISPLAY}" in
  *wayland*)
    echo "note: Wayland — при старте будет диалог KDE «разрешить захват экрана»"
    command -v gst-launch-1.0 >/dev/null 2>&1 || echo "warn: gst-launch-1.0 не найден — видео на Wayland не пойдёт (звук/ввод будут)"
    ;;
esac

# Ввод: uinput (X11 полностью; на Wayland мышь через портал, клавиатура — uinput).
if [ ! -w /dev/uinput ]; then
  echo "warn: нет доступа к /dev/uinput — ВВОД не заработает. Разово (sudo):"
  echo "        sudo modprobe uinput && sudo chgrp \"$(id -gn)\" /dev/uinput && sudo chmod 660 /dev/uinput"
fi

echo "run (session $SESSION)…"
exec "$BIN" --session "$SESSION" --broker "$BROKER" --audio
