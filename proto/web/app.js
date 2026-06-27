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

// pc и ws пересоздаются при смене кодека (новый трек = ренеготиация).
let pc = null;
let ws = null;

// --- Настройки (модель для lil-gui) ---

const settings = {
  codec: "vp8", // vp8 (software) | h264 (VideoToolbox HW)
  source: "", // "screen:<idx>" | "window:<id>" | "app:<pid>"
  width: 1280, // ширина картинки, px; 0 = нативное
  fps: 30,
  bitrate: 3000, // kbps
  threads: 0, // потоки энкодера ffmpeg; 0 = авто
  dropLate: false, // выкидывать старые кадры под нагрузкой
  buffer: 0, // целевой джиттер-буфер приёмника, мс
  audio: false, // передавать звук (SCK → Opus); connect-time
  volume: 1, // громкость воспроизведения 0..1 (клиент)
  muted: true, // mute воспроизведения (клиент; true для автоплея)
  control: false, // управлять мышью хоста (отправлять события)
};

// Восстанавливаем сохранённые настройки до постройки панели, чтобы контролы
// сразу показали их. Стартовые значения уходят в query при коннекте.
const STORAGE_KEY = "katana.settings";
try {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (raw) {
    const saved = JSON.parse(raw);
    // берём только известные ключи того же типа — на случай смены схемы
    for (const k of Object.keys(settings)) {
      if (typeof saved[k] === typeof settings[k]) settings[k] = saved[k];
    }
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

// Разбирает settings.source ("kind:id") в параметры захвата для сервера.
// Всё идёт через ScreenCaptureKit: display/window/app по числовому id.
function sourceParams() {
  const [kind, idStr] = (settings.source || "display:0").split(":");
  return { sourceKind: kind, sourceId: parseInt(idStr, 10) || 0 };
}

// Живые метрики (readonly-поля панели).
const metrics = {
  res: "—",
  fps: "—",
  encoder: "—",
  latency: "—",
};

// Громкость/mute воспроизведения — клиентские, мгновенные (на <video>).
function applyAudioPlayback() {
  video.volume = settings.volume;
  video.muted = settings.muted;
}

// jitterBufferTarget + playoutDelayHint — клиентские рычаги задержки приёма.
// Применяем к ОБОИМ трекам: при наличии аудио Chrome синхронизирует A/V и
// подтягивает видео под звук, раздувая видео-буфер; playoutDelayHint=0 просит
// минимальную задержку проигрывания и противодействует этому.
function applyBufferTarget() {
  if (!pc) return;
  for (const r of pc.getReceivers()) {
    if (!r.track) continue;
    try {
      if ("jitterBufferTarget" in r) r.jitterBufferTarget = settings.buffer;
      if ("playoutDelayHint" in r) r.playoutDelayHint = settings.buffer / 1000; // сек
    } catch (err) {
      console.warn("buffer hint:", err);
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
    const config = {
      width: settings.width,
      fps: settings.fps,
      bitrateKbps: settings.bitrate,
      threads: settings.threads,
      dropLate: settings.dropLate,
      cursor: !settings.control, // при управлении прячем курсор хоста в захвате
      ...sourceParams(),
    };
    send({ type: "config", config });
    setStatus("applying settings…");
    // Состояние соединения при reconfig не меняется, поэтому надпись надо
    // спрятать самим — примерно когда ffmpeg перезапустился и кадры пошли.
    clearTimeout(statusTimer);
    statusTimer = setTimeout(() => {
      if (pc && pc.connectionState === "connected") setStatus("live", true);
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
video.addEventListener("dblclick", () => {
  if (!settings.control) toggleFullscreen(); // в режиме управления dblclick = два клика
});
window.addEventListener("keydown", (e) => {
  if (inPanel(document.activeElement)) return; // не мешаем работе с панелью
  if (e.key === "f" || e.key === "F") toggleFullscreen();
});

// --- Разрешения macOS ---
const perms = { screen: "—", control: "—" };

async function refreshPerms() {
  try {
    const j = await (await fetch("/api/permissions")).json();
    perms.screen = j.screen ? "✓ granted" : "✗ denied";
    perms.control = j.accessibility ? "✓ granted" : "✗ denied";
  } catch (err) {
    console.warn("permissions:", err);
  }
}
async function requestPerm(path) {
  try {
    await fetch("/api/permissions/" + path, { method: "POST" });
  } catch (err) {
    console.warn("permissions:", err);
  }
  await refreshPerms();
}
function openPermSettings(target) {
  fetch("/api/permissions/open?target=" + target, { method: "POST" }).catch(() => {});
}

// Кладёт пару кнопок в один ряд (lil-gui по умолчанию стакает вертикально).
function buttonRow(folder, buttons) {
  const row = document.createElement("div");
  row.className = "btn-row";
  for (const b of buttons) {
    const el = document.createElement("button");
    el.textContent = b.label;
    el.addEventListener("click", b.onClick);
    row.appendChild(el);
  }
  folder.$children.appendChild(row);
}

const perm = gui.addFolder("Permissions · macOS");
perm.domElement.classList.add("f-perms");
perm.add(perms, "screen").name("Screen recording").disable().listen();
buttonRow(perm, [
  { label: "Request", onClick: () => requestPerm("screen") },
  { label: "Open settings", onClick: () => openPermSettings("screen") },
]);
perm.add(perms, "control").name("Control · a11y").disable().listen();
buttonRow(perm, [
  { label: "Request", onClick: () => requestPerm("accessibility") },
  { label: "Open settings", onClick: () => openPermSettings("accessibility") },
]);
refreshPerms();
setInterval(refreshPerms, 5000);

const cap = gui.addFolder("Capture · ffmpeg");
cap.domElement.classList.add("f-capture");
cap
  .add(settings, "codec", { "VP8 · software": "vp8", "H264 · VideoToolbox (HW)": "h264" })
  .name("Encoder")
  .onChange(() => {
    saveSettings();
    connect(); // смена кодека = новый трек → переподключаемся
  });
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
cap.add(settings, "audio").name("Audio (transmit)").onChange(() => {
  saveSettings();
  connect(); // вкл/выкл звука = добавить/убрать дорожку → переподключение
});

const recv = gui.addFolder("Receive · browser");
recv.domElement.classList.add("f-receive");
recv
  .add(settings, "buffer", { "0 · min latency": 0, "50 ms": 50, "100 ms": 100, "200 ms · smoother": 200 })
  .name("Jitter buffer")
  .onChange(() => {
    applyBufferTarget();
    saveSettings();
  });
recv.add(settings, "volume", 0, 1, 0.05).name("Volume").onChange(() => {
  applyAudioPlayback();
  saveSettings();
});
recv.add(settings, "muted").name("Mute").onChange(() => {
  applyAudioPlayback();
  saveSettings();
});
recv.add(settings, "control").name("Control (mouse)").onChange(() => {
  saveSettings();
  sendConfig(); // обновить showsCursor на сервере (прятать курсор хоста при управлении)
});

const stat = gui.addFolder("Stats");
stat.domElement.classList.add("f-stats");
stat.add(metrics, "res").name("Resolution").disable().listen();
stat.add(metrics, "fps").name("FPS (real/target)").disable().listen();
stat.add(metrics, "encoder").name("Encoder").disable().listen();
stat.add(metrics, "latency").name("Latency").disable().listen();

// --- WebRTC ---

function send(msg) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

// Стартовые настройки уходят в query — сервер сразу запускает захват с ними
// (без лишнего reconfig после коннекта). Кодек меняется только так.
function connectURL() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const q = new URLSearchParams({
    codec: settings.codec,
    width: settings.width,
    fps: settings.fps,
    bitrateKbps: settings.bitrate,
    threads: settings.threads,
    dropLate: settings.dropLate,
    audio: settings.audio,
    cursor: !settings.control,
    ...sourceParams(),
  });
  return `${proto}://${location.host}/ws?${q}`;
}

function disconnect() {
  if (ws) {
    ws.onclose = null; // не показывать "server unavailable" при намеренном закрытии
    ws.close();
    ws = null;
  }
  if (pc) {
    pc.close();
    pc = null;
  }
}

function connect() {
  disconnect();

  // Пустой конфиг: localhost, host-кандидаты, без ICE-серверов.
  pc = new RTCPeerConnection();
  window.pc = pc; // для отладочной статистики из DevTools

  pc.ontrack = (event) => {
    applyBufferTarget(); // применить текущий выбор буфера к новому приёмнику
    if (video.srcObject !== event.streams[0]) {
      video.srcObject = event.streams[0];
    }
    applyAudioPlayback(); // громкость/mute
  };

  pc.onconnectionstatechange = () => {
    if (!pc) return;
    switch (pc.connectionState) {
      case "connected":
        setStatus("live", true);
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

  pc.onicecandidate = (event) => {
    if (event.candidate) {
      send({ type: "candidate", candidate: event.candidate.toJSON() });
    }
  };

  ws = new WebSocket(connectURL());
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
}

// --- Живые метрики ---
// ВНИМАНИЕ: задержка здесь — только WebRTC-половина (буфер + декод + RTT).
// Захват и энкод ffmpeg происходят ДО RTP и сюда не входят (см. README).
//
// Считаем МГНОВЕННУЮ задержку буфера/декода — дельты между замерами, а не
// накопительное среднее за сессию. Иначе смена джиттер-буфера не видна:
// среднее за минуты почти не реагирует на изменение «здесь и сейчас».
// Watchdog зависания видео: сколько секунд подряд framesDecoded не растёт.
const WD_STALL_LIMIT = 3;
let wd = { last: -1, stall: 0 };

let prev = null;
setInterval(async () => {
  if (!pc || pc.connectionState !== "connected") {
    metrics.res = metrics.fps = metrics.encoder = metrics.latency = "—";
    prev = null;
    wd.last = -1;
    wd.stall = 0;
    return;
  }
  let inbound = null, pair = null, remote = null;
  (await pc.getStats()).forEach((r) => {
    if (r.type === "inbound-rtp" && r.kind === "video") inbound = r;
    if (r.type === "candidate-pair" && r.nominated) pair = r;
    if (r.type === "remote-inbound-rtp" && r.kind === "video") remote = r;
  });

  // Watchdog зависания видео: декодированные кадры перестали расти при живом
  // соединении (на статике они всё равно растут — тикер повторяет кадр). Если
  // встало на несколько секунд — бесшовно переподключаемся (как ручной reload).
  if (inbound && inbound.framesDecoded != null && video.videoWidth > 0) {
    if (inbound.framesDecoded === wd.last) {
      if (++wd.stall >= WD_STALL_LIMIT) {
        console.warn("video frozen → reconnecting");
        setStatus("video stalled, reconnecting…");
        wd.last = -1;
        wd.stall = 0;
        connect();
        return;
      }
    } else {
      wd.stall = 0;
      wd.last = inbound.framesDecoded;
    }
  }

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

// Добавляет опцию с уникальным ключом (lil-gui не любит дубли меток).
function addOpt(opts, label, value) {
  let key = label;
  for (let n = 2; key in opts; n++) key = `${label} (${n})`;
  opts[key] = value;
}

// Единый список источников: экраны (avfoundation) + окна и приложения (SCK).
// Делаем до connect(), чтобы первый захват сразу пошёл с нужного источника.
async function initSources() {
  const src = await fetch("/api/sources")
    .then((r) => r.json())
    .catch(() => ({ displays: [], windows: [], apps: [] }));

  const options = {};
  for (const d of src.displays || []) {
    addOpt(options, `Screen · Display ${d.id} (${d.width}×${d.height})`, `display:${d.id}`);
  }
  for (const w of src.windows || []) {
    addOpt(options, `Win · ${w.app}: ${w.title}`.slice(0, 58), `window:${w.id}`);
  }
  for (const a of src.apps || []) addOpt(options, `App · ${a.name}`, `app:${a.pid}`);

  const values = Object.values(options);
  if (!values.includes(settings.source)) {
    settings.source = src.displays && src.displays[0] ? `display:${src.displays[0].id}` : values[0] || "display:0";
  }
  if (!values.length) addOpt(options, "(none)", "display:0");

  const ctrl = cap.add(settings, "source", options).name("Source").onChange(sendConfig);
  // ставим Source сразу после Encoder (перед Width) — выбор источника важнее.
  const widthEl = cap.controllers.find((c) => c.property === "width")?.domElement;
  if (widthEl) cap.$children.insertBefore(ctrl.domElement, widthEl);
}

// --- Управление мышью ---
// Координаты курсора над видео → нормализованные [0,1] с учётом object-fit:
// contain (letterbox). Сервер мапит их в глобальные координаты источника.
function videoCoords(ev) {
  const r = video.getBoundingClientRect();
  const vw = video.videoWidth, vh = video.videoHeight;
  if (!vw || !vh) return null;
  const scale = Math.min(r.width / vw, r.height / vh);
  const dispW = vw * scale, dispH = vh * scale;
  const nx = (ev.clientX - r.left - (r.width - dispW) / 2) / dispW;
  const ny = (ev.clientY - r.top - (r.height - dispH) / 2) / dispH;
  if (nx < 0 || nx > 1 || ny < 0 || ny > 1) return null; // вне картинки (letterbox)
  return { x: nx, y: ny };
}

let lastCoords = null;
function sendMouse(ev, action) {
  if (!settings.control) return;
  const c = videoCoords(ev) || (action === "up" ? lastCoords : null);
  if (!c) return;
  lastCoords = c;
  send({ type: "mouse", mouse: { x: c.x, y: c.y, action, button: ev.button === 2 ? "right" : "left" } });
}

let lastMove = 0;
video.addEventListener("mousemove", (ev) => {
  if (!settings.control) return;
  if (ev.timeStamp - lastMove < 16) return; // ~60/с
  lastMove = ev.timeStamp;
  sendMouse(ev, "move");
});
video.addEventListener("mousedown", (ev) => {
  if (settings.control) ev.preventDefault();
  sendMouse(ev, "down");
});
// mouseup ловим на window: отпускание долетит, даже если курсор ушёл за видео
// (иначе drag «залипнет» с зажатой кнопкой на хосте).
window.addEventListener("mouseup", (ev) => sendMouse(ev, "up"));

// Старт.
(async () => {
  await initSources();
  connect();
})();
