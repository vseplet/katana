#!/bin/sh
# Запуск нативного хоста katana на Steam Deck: качает готовый бинарь из CI и
# стартует с тестовой сессией. Go/сборка на Deck больше не нужны.
#
#   sh steamos.sh
#
# Бинарь собирается воркфлоу steamos-dev (Actions → Run workflow) в Debian 12
# (портативный glibc) и публикуется в rolling-пре-релиз с тегом native.
set -e

SESSION="5ca6efe2-3e5d-43f3-b92b-c3966a196fec"
BROKER="wss://katana.vseplet.deno.net/rtc"

KAT="$HOME/.katana"
BIN="$KAT/bin/katana-native"
LIBS="$KAT/native-libs"
REL="https://github.com/vseplet/katana/releases/download/native"
mkdir -p "$KAT/bin"

echo "downloading native build + ffmpeg libs…"
curl -fL "$REL/katana-steamos-amd64" -o "$BIN" || { echo "error: не скачался бинарь — сначала запусти workflow steamos-dev (Actions → Run workflow)"; exit 1; }
curl -fL "$REL/native-libs.tar.gz" -o "$KAT/native-libs.tar.gz" || { echo "error: не скачались либы"; exit 1; }
rm -rf "$LIBS"
tar -C "$KAT" -xzf "$KAT/native-libs.tar.gz"  # → $KAT/native-libs/
chmod +x "$BIN"
# Забандленные ffmpeg-либы имеют приоритет (не зависим от версии ffmpeg на Deck).
export LD_LIBRARY_PATH="$LIBS:$LD_LIBRARY_PATH"

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
