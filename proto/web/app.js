// Фронтенд-зритель: роль answerer. Go офферит, мы отвечаем (см. §4 ТЗ).
// Панель управления на lil-gui: захват настраивается на лету.

import GUI from "./lil-gui.esm.min.js";

const video = document.getElementById("screen");
const statusEl = document.getElementById("status");

function setStatus(text, hide = false) {
  statusEl.textContent = text;
  statusEl.style.opacity = hide ? "0" : "1";
}

// --- Блокировка случайных жестов масштабирования/прокрутки ---
// Особенно важно на мобильных. Панель lil-gui исключаем, чтобы она работала.
const inPanel = (target) => !!(target && target.closest && target.closest(".lil-gui"));

// iOS Safari pinch-zoom (нестандартные gesture-события).
for (const ev of ["gesturestart", "gesturechange", "gestureend"]) {
  document.addEventListener(ev, (e) => e.preventDefault(), { passive: false });
}

// Ctrl/⌘ + колесо = зум страницы на десктопе (трекпад-щипок шлёт ctrlKey).
window.addEventListener(
  "wheel",
  (e) => {
    if (e.ctrlKey || e.metaKey) e.preventDefault();
  },
  { passive: false }
);

// Двойной тап для зума (iOS) — глушим вне панели.
let lastTap = 0;
document.addEventListener(
  "touchend",
  (e) => {
    if (inPanel(e.target)) return;
    const now = e.timeStamp;
    if (now - lastTap < 350) e.preventDefault();
    lastTap = now;
  },
  { passive: false }
);

// Контекстное меню / iOS callout по долгому нажатию — вне панели.
document.addEventListener("contextmenu", (e) => {
  if (!inPanel(e.target)) e.preventDefault();
});

// Пустой конфиг: localhost, host-кандидаты, без ICE-серверов.
const pc = new RTCPeerConnection();
window.pc = pc; // для отладочной статистики из DevTools

// --- Настройки (модель для lil-gui) ---

const settings = {
  width: 1280, // ширина картинки, px; 0 = нативное
  fps: 30,
  bitrate: 3000, // kbps
  threads: 0, // потоки энкодера ffmpeg; 0 = авто
  dropLate: false, // выкидывать старые кадры под нагрузкой
  buffer: 0, // целевой джиттер-буфер приёмника, мс
};

// Восстанавливаем сохранённые настройки до постройки панели, чтобы контролы
// сразу показали их. hadSaved → после коннекта синхронизируем сервер под них.
const STORAGE_KEY = "katana.settings";
let hadSaved = false;
try {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (raw) {
    const saved = JSON.parse(raw);
    // берём только известные ключи того же типа — на случай смены схемы
    for (const k of Object.keys(settings)) {
      if (typeof saved[k] === typeof settings[k]) settings[k] = saved[k];
    }
    hadSaved = true;
  }
} catch (err) {
  console.warn("settings load:", err);
}

function saveSettings() {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(settings));
  } catch (err) {
    console.warn("settings save:", err);
  }
}

// Живые метрики (readonly-поля панели).
const metrics = {
  res: "—",
  fps: "—",
  encoder: "—",
  latency: "—",
};

// jitterBufferTarget — клиентский рычаг, применяется мгновенно (без ffmpeg).
function applyBufferTarget() {
  for (const r of pc.getReceivers()) {
    if (r.track && r.track.kind === "video" && "jitterBufferTarget" in r) {
      try {
        r.jitterBufferTarget = settings.buffer;
      } catch (err) {
        console.warn("jitterBufferTarget:", err);
      }
    }
  }
}

// Дебаунс серверного reconfig: ffmpeg перезапускается, не дёргаем на каждый шаг.
let cfgTimer = null;
let statusTimer = null;
function sendConfig() {
  saveSettings();
  clearTimeout(cfgTimer);
  cfgTimer = setTimeout(() => {
    send({
      type: "config",
      config: {
        width: settings.width,
        fps: settings.fps,
        bitrateKbps: settings.bitrate,
        threads: settings.threads,
        dropLate: settings.dropLate,
      },
    });
    setStatus("applying settings…");
    // Состояние соединения при reconfig не меняется, поэтому надпись надо
    // спрятать самим — примерно когда ffmpeg перезапустился и кадры пошли.
    clearTimeout(statusTimer);
    statusTimer = setTimeout(() => {
      if (pc.connectionState === "connected") setStatus("live", true);
    }, 1800);
  }, 250);
}

// --- Панель lil-gui ---

const gui = new GUI({ title: "katana", width: 310 });
window.gui = gui; // для отладки из DevTools

// Фуллскрин: кнопка в панели, двойной клик по видео и клавиша F.
// requestFullscreen требует жеста пользователя — все три варианта им являются.
function toggleFullscreen() {
  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else {
    document.documentElement.requestFullscreen().catch((err) => console.warn("fullscreen:", err));
  }
}
gui.add({ fullscreen: toggleFullscreen }, "fullscreen").name("Fullscreen ⤢");
video.addEventListener("dblclick", toggleFullscreen);
window.addEventListener("keydown", (e) => {
  if (inPanel(document.activeElement)) return; // не мешаем работе с панелью
  if (e.key === "f" || e.key === "F") toggleFullscreen();
});

const cap = gui.addFolder("Capture · ffmpeg");
cap.domElement.classList.add("f-capture");
cap
  .add(settings, "width", {
    "640 px": 640,
    "960 px": 960,
    "1280 px": 1280,
    "1600 px": 1600,
    "1920 px": 1920,
    "2560 px": 2560,
    "3840 px · 4K": 3840,
    "native": 0,
  })
  .name("Width")
  .onChange(sendConfig);
cap.add(settings, "fps", [15, 24, 30, 60]).name("Frame rate").onChange(sendConfig);
cap
  .add(settings, "bitrate", { "1 Mbps": 1000, "2 Mbps": 2000, "3 Mbps": 3000, "6 Mbps": 6000 })
  .name("Quality")
  .onChange(sendConfig);
cap
  .add(settings, "threads", { auto: 0, "1": 1, "2": 2, "4": 4, "8": 8 })
  .name("Encoder threads")
  .onChange(sendConfig);
cap.add(settings, "dropLate").name("Drop late frames").onChange(sendConfig);

const recv = gui.addFolder("Receive · browser");
recv.domElement.classList.add("f-receive");
recv
  .add(settings, "buffer", { "0 · min latency": 0, "50 ms": 50, "100 ms": 100, "200 ms · smoother": 200 })
  .name("Jitter buffer")
  .onChange(() => {
    applyBufferTarget();
    saveSettings();
  });

const stat = gui.addFolder("Stats");
stat.domElement.classList.add("f-stats");
stat.add(metrics, "res").name("Resolution").disable().listen();
stat.add(metrics, "fps").name("FPS (real/target)").disable().listen();
stat.add(metrics, "encoder").name("Encoder").disable().listen();
stat.add(metrics, "latency").name("Latency").disable().listen();

// --- WebRTC ---

pc.ontrack = (event) => {
  applyBufferTarget(); // применить текущий выбор буфера к новому приёмнику
  if (video.srcObject !== event.streams[0]) {
    video.srcObject = event.streams[0];
  }
};

let syncedOnce = false;
pc.onconnectionstatechange = () => {
  switch (pc.connectionState) {
    case "connected":
      setStatus("live", true);
      // Сервер стартует со своих дефолтов. Если есть сохранённые настройки —
      // один раз досылаем config, чтобы поток совпал с тем, что в панели.
      if (hadSaved && !syncedOnce) {
        syncedOnce = true;
        sendConfig();
      }
      break;
    case "connecting":
      setStatus("connecting…");
      break;
    case "failed":
      setStatus("connection failed");
      break;
    case "disconnected":
      setStatus("reconnecting…");
      break;
  }
};

const proto = location.protocol === "https:" ? "wss" : "ws";
const ws = new WebSocket(`${proto}://${location.host}/ws`);

pc.onicecandidate = (event) => {
  if (event.candidate) {
    send({ type: "candidate", candidate: event.candidate.toJSON() });
  }
};

function send(msg) {
  if (ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

ws.onopen = () => setStatus("waiting for offer…");
ws.onclose = () => setStatus("server unavailable");
ws.onerror = () => setStatus("WebSocket error");

ws.onmessage = async (event) => {
  const msg = JSON.parse(event.data);
  switch (msg.type) {
    case "offer": {
      await pc.setRemoteDescription({ type: "offer", sdp: msg.sdp });
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      send({ type: "answer", sdp: answer.sdp });
      break;
    }
    case "candidate": {
      try {
        await pc.addIceCandidate(msg.candidate);
      } catch (err) {
        console.error("addIceCandidate:", err);
      }
      break;
    }
    default:
      console.warn("неизвестное сообщение:", msg.type);
  }
};

// --- Живые метрики ---
// ВНИМАНИЕ: задержка здесь — только WebRTC-половина (буфер + декод + RTT).
// Захват и энкод ffmpeg происходят ДО RTP и сюда не входят (см. README).
//
// Считаем МГНОВЕННУЮ задержку буфера/декода — дельты между замерами, а не
// накопительное среднее за сессию. Иначе смена джиттер-буфера не видна:
// среднее за минуты почти не реагирует на изменение «здесь и сейчас».
let prev = null;
setInterval(async () => {
  if (pc.connectionState !== "connected") {
    metrics.res = metrics.fps = metrics.encoder = metrics.latency = "—";
    prev = null;
    return;
  }
  let inbound = null, pair = null, remote = null;
  (await pc.getStats()).forEach((r) => {
    if (r.type === "inbound-rtp" && r.kind === "video") inbound = r;
    if (r.type === "candidate-pair" && r.nominated) pair = r;
    if (r.type === "remote-inbound-rtp" && r.kind === "video") remote = r;
  });

  metrics.res = video.videoWidth ? `${video.videoWidth}×${video.videoHeight}` : "—";

  // FPS: фактический против целевого. Энкодер перегружен, если реальный fps
  // заметно ниже цели ИЛИ кадры приходят рывками (джиттер прихода больше
  // интервала кадра) — софт-VP8 не успевает кодировать в realtime.
  const targetFps = settings.fps;
  const actualFps = inbound && inbound.framesPerSecond != null ? inbound.framesPerSecond : null;
  metrics.fps = actualFps != null ? `${Math.round(actualFps)} / ${targetFps}` : "—";

  const jitterMs = inbound && inbound.jitter != null ? inbound.jitter * 1000 : null;
  if (actualFps == null || jitterMs == null) {
    metrics.encoder = "—";
  } else {
    const overloaded = actualFps < targetFps * 0.9 || jitterMs > 1000 / targetFps;
    metrics.encoder = `${overloaded ? "⚠ overload" : "ok"} · jitter ${Math.round(jitterMs)} ms`;
  }

  if (inbound) {
    const cur = {
      jbDelay: inbound.jitterBufferDelay || 0,
      jbCount: inbound.jitterBufferEmittedCount || 0,
      decTime: inbound.totalDecodeTime || 0,
      decCount: inbound.framesDecoded || 0,
    };
    const rttSec =
      (pair && pair.currentRoundTripTime != null ? pair.currentRoundTripTime : remote && remote.roundTripTime) || 0;
    const rtt = rttSec * 1000;

    if (prev && cur.jbCount > prev.jbCount) {
      const jb = ((cur.jbDelay - prev.jbDelay) / (cur.jbCount - prev.jbCount)) * 1000;
      const dec =
        cur.decCount > prev.decCount
          ? ((cur.decTime - prev.decTime) / (cur.decCount - prev.decCount)) * 1000
          : 0;
      const est = jb + dec + rtt / 2;
      metrics.latency = `~${Math.round(est)} ms (buf ${Math.round(jb)}+dec ${Math.round(dec)}+RTT/2 ${Math.round(rtt / 2)})`;
    }
    prev = cur;
  } else {
    metrics.latency = "—";
    prev = null;
  }
}, 1000);
