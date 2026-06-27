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
let inputDC = null; // DataChannel "input" (mouse/scroll/cursor); создаёт сервер

// --- Настройки (модель для lil-gui) ---

const settings = {
  codec: "vp8", // vp8 (software) | h264 (VideoToolbox HW)
  source: "", // "screen:<idx>" | "window:<id>" | "app:<pid>"
  width: 1280, // ширина картинки, px; 0 = нативное
  fps: 30,
  bitrate: 3000, // kbps
  threads: 0, // потоки энкодера ffmpeg; 0 = авто
  dropLate: false, // выкидывать старые кадры под нагрузкой
  buffer: -1, // джиттер-буфер приёмника, мс; -1 = auto (адаптивный, для сети)
  audio: false, // передавать звук (SCK → Opus); connect-time
  volume: 1, // громкость воспроизведения 0..1 (клиент)
  muted: true, // mute воспроизведения (клиент; true для автоплея)
  control: false, // управлять мышью хоста (отправлять события)
  // тюнинг ввода (клиентский):
  scrollSpeed: 1, // множитель скролла; 1 = пиксель-в-пиксель (1:1, как трекпад)
  invertScrollX: false,
  invertScrollY: false,
  dragDeadzone: 8, // px порога, после которого тап превращается в drag
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

// Состояние свёрнутости секций панели — отдельно от настроек захвата.
const UI_KEY = "katana.ui";
let uiState = {};
try {
  uiState = JSON.parse(localStorage.getItem(UI_KEY) || "{}");
} catch (err) {
  console.warn("ui state load:", err);
}

// persistFolder восстанавливает свёрнутость папки и сохраняет её при изменении.
// Если состояния нет в localStorage — применяем defaultClosed.
function persistFolder(folder, name, defaultClosed = false) {
  const closed = name in uiState ? uiState[name] : defaultClosed;
  closed ? folder.close() : folder.open();
  folder.onOpenClose((f) => {
    uiState[name] = f._closed;
    try {
      localStorage.setItem(UI_KEY, JSON.stringify(uiState));
    } catch (err) {
      console.warn("ui state save:", err);
    }
  });
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

// Гарантируем проигрывание. Если autoplay со звуком заблокирован браузером
// (muted=false), форсим muted и пробуем снова — немой автоплей всегда разрешён,
// так видео точно появляется без жеста (раньше помогали reload/фуллскрин).
function tryPlay() {
  if (!video.srcObject) return;
  const p = video.play();
  if (p && p.catch) {
    p.catch(() => {
      video.muted = true;
      video.play().catch(() => {});
    });
  }
}

// jitterBufferTarget + playoutDelayHint — клиентские рычаги задержки приёма.
// Применяем к ОБОИМ трекам: при наличии аудио Chrome синхронизирует A/V и
// подтягивает видео под звук, раздувая видео-буфер; playoutDelayHint=0 просит
// минимальную задержку проигрывания и противодействует этому.
function applyBufferTarget() {
  if (!pc) return;
  // auto (<0): не форсим — Chrome сам растит буфер под джиттер/потери сети.
  // Фиксированное значение (0/50/…): форсим (0 = минимум, только для loopback).
  const auto = settings.buffer < 0;
  for (const r of pc.getReceivers()) {
    if (!r.track) continue;
    try {
      if ("jitterBufferTarget" in r) r.jitterBufferTarget = auto ? null : settings.buffer;
      if ("playoutDelayHint" in r) r.playoutDelayHint = auto ? null : settings.buffer / 1000;
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
  .add(settings, "buffer", {
    "auto · adaptive": -1,
    "0 · min latency": 0,
    "50 ms": 50,
    "100 ms": 100,
    "200 ms · smoother": 200,
  })
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

// Тюнинг ввода (клиентский) — чтобы подобрать дефолты по ощущениям.
const inp = gui.addFolder("Input · tuning");
inp.domElement.classList.add("f-input");
inp.add(settings, "scrollSpeed", 0.2, 3, 0.1).name("Scroll speed").onChange(saveSettings);
inp.add(settings, "invertScrollX").name("Invert scroll X").onChange(saveSettings);
inp.add(settings, "invertScrollY").name("Invert scroll Y").onChange(saveSettings);
inp.add(settings, "dragDeadzone", 0, 30, 1).name("Drag deadzone px").onChange(saveSettings);

const stat = gui.addFolder("Stats");
stat.domElement.classList.add("f-stats");
stat.add(metrics, "res").name("Resolution").disable().listen();
stat.add(metrics, "fps").name("FPS (real/target)").disable().listen();
stat.add(metrics, "encoder").name("Encoder").disable().listen();
stat.add(metrics, "latency").name("Latency").disable().listen();

// Восстановить/сохранять свёрнутость секций. По умолчанию подсекции свёрнуты
// (root открыт, чтобы заголовки были видны); сохранённое состояние переопределяет.
persistFolder(gui, "root", false);
persistFolder(cap, "capture", true);
persistFolder(recv, "receive", true);
persistFolder(inp, "input", true);
persistFolder(stat, "stats", true);
persistFolder(perm, "perms", true);

// --- WebRTC ---

function send(msg) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

// Ввод (mouse/scroll/cursor) — по DataChannel; пока он не открыт, фолбэк на WS.
function sendInput(msg) {
  if (inputDC && inputDC.readyState === "open") {
    inputDC.send(JSON.stringify(msg));
  } else {
    send(msg);
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
  inputDC = null;
}

function connect() {
  disconnect();

  // Пустой конфиг: localhost, host-кандидаты, без ICE-серверов.
  pc = new RTCPeerConnection();
  window.pc = pc; // для отладочной статистики из DevTools

  // Канал ввода создаёт сервер (офферер) — ловим его здесь.
  pc.ondatachannel = (e) => {
    if (e.channel.label === "input") inputDC = e.channel;
  };

  pc.ontrack = (event) => {
    applyBufferTarget(); // применить текущий выбор буфера к новому приёмнику
    if (video.srcObject !== event.streams[0]) {
      video.srcObject = event.streams[0];
    }
    applyAudioPlayback(); // громкость/mute
    tryPlay(); // явно запускаем — не полагаемся на autoplay
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
        // Сеть разорвалась и ICE не восстановился сам (бывает на Tailscale) —
        // переподключаемся. Небольшая задержка, чтобы не молотить.
        setStatus("connection failed, reconnecting…");
        setTimeout(() => {
          if (pc && pc.connectionState === "failed") connect();
        }, 1000);
        break;
      case "disconnected":
        setStatus("reconnecting…"); // ICE часто восстанавливается сам
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
  // Самолечение: соединение живо, но <video> на паузе (autoplay не стартовал) —
  // пробуем запустить (немой fallback внутри). Лечит «нет видео при старте».
  if (video.srcObject && video.paused) tryPlay();

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
function sendMouseAt(clientX, clientY, action, button) {
  const c = videoCoords({ clientX, clientY }) || (action === "up" ? lastCoords : null);
  if (!c) return;
  lastCoords = c;
  sendInput({ type: "mouse", mouse: { x: c.x, y: c.y, action, button } });
}

// Нажатие с дедзоной (settings.dragDeadzone): move не шлём, пока не сдвинулись
// больше порога от точки down. Иначе на тач любой тап дёргается → drag.
let press = null; // { x, y, button, moved }

function ctrlDown(clientX, clientY, button) {
  press = { x: clientX, y: clientY, button, moved: false };
  sendMouseAt(clientX, clientY, "down", button);
}
function ctrlMove(clientX, clientY) {
  if (!press) {
    sendMouseAt(clientX, clientY, "move", "left"); // hover мышью (кнопка не зажата)
    return;
  }
  if (!press.moved) {
    if (Math.hypot(clientX - press.x, clientY - press.y) < settings.dragDeadzone) return; // дедзона
    press.moved = true;
  }
  sendMouseAt(clientX, clientY, "move", press.button);
}
function ctrlUp(clientX, clientY, button) {
  if (press && !press.moved) {
    clientX = press.x; // не двигались дальше дедзоны → отпускаем точно в точке нажатия
    clientY = press.y;
  }
  sendMouseAt(clientX, clientY, "up", button);
  press = null;
}

// Скролл пиксель-в-пиксель (как трекпад): шлём ровно столько пикселей, на
// сколько свайпнули × scrollSpeed. Дробный остаток копим, чтобы не терять.
let scrollAcc = { x: 0, y: 0 };
function sendScroll(dxPx, dyPx) {
  const s = settings.scrollSpeed;
  scrollAcc.x += dxPx * s;
  scrollAcc.y += dyPx * s;
  let dx = Math.trunc(scrollAcc.x);
  let dy = Math.trunc(scrollAcc.y);
  if (!dx && !dy) return;
  scrollAcc.x -= dx;
  scrollAcc.y -= dy;
  if (settings.invertScrollX) dx = -dx;
  if (settings.invertScrollY) dy = -dy;
  sendInput({ type: "scroll", scroll: { dx, dy } });
}

// --- Вьюпорт: зум и пан (клиентские, через CSS-transform видео) ---
// Маппинг координат для управления (videoCoords) берёт getBoundingClientRect,
// который УЖЕ учитывает transform — поэтому клики попадают верно и при зуме.
const view = { zoom: 1, panX: 0, panY: 0 };

function clampPan() {
  const w = window.innerWidth, h = window.innerHeight;
  view.panX = Math.min(0, Math.max(w * (1 - view.zoom), view.panX));
  view.panY = Math.min(0, Math.max(h * (1 - view.zoom), view.panY));
}
function applyView() {
  clampPan();
  video.style.transform = `translate(${view.panX}px, ${view.panY}px) scale(${view.zoom})`;
}
function resetView() {
  view.zoom = 1;
  view.panX = 0;
  view.panY = 0;
  applyView();
}
// Зум к точке экрана (cx,cy): точка под курсором/пальцем остаётся на месте.
function zoomAt(cx, cy, factor) {
  const z0 = view.zoom;
  const z1 = Math.min(5, Math.max(1, z0 * factor));
  if (z1 === z0) return;
  view.panX = cx - (cx - view.panX) * (z1 / z0);
  view.panY = cy - (cy - view.panY) * (z1 / z0);
  view.zoom = z1;
  applyView();
}

// --- Режим управления ---
const btnControl = document.getElementById("btn-control");
function setControl(on) {
  settings.control = on;
  btnControl.classList.toggle("active", on);
  video.style.cursor = on ? "crosshair" : "default";
  updateAppsVisibility(); // ленту приложений прячем в режиме управления
  saveSettings();
  // Курсор хоста меняем на лету (без перезапуска захвата → без обрыва видео).
  sendInput({ type: "cursor", config: { cursor: !on } });
}

// --- Лента открытых приложений ---
const appsEl = document.getElementById("apps");
async function refreshApps() {
  if (settings.control) return; // в режиме управления лента скрыта
  try {
    const src = await fetch("/api/sources").then((r) => r.json());
    appsEl.innerHTML = "";
    for (const a of src.apps || []) {
      const b = document.createElement("button");
      b.textContent = a.name;
      b.addEventListener("click", () => {
        fetch("/api/activate?pid=" + a.pid, { method: "POST" }).catch(() => {});
      });
      appsEl.appendChild(b);
    }
  } catch (err) {
    console.warn("apps:", err);
  }
}
function updateAppsVisibility() {
  appsEl.classList.toggle("hidden", settings.control);
  if (!settings.control) refreshApps();
}
setInterval(refreshApps, 5000); // refreshApps сам не делает ничего в режиме управления

// --- Ввод: в режиме control → на хост, иначе → навигация вьюпорта ---
let lastMove = 0;
let panning = null; // мышиный пан в режиме просмотра

video.addEventListener("mousedown", (ev) => {
  ev.preventDefault();
  if (settings.control) {
    ctrlDown(ev.clientX, ev.clientY, ev.button === 2 ? "right" : "left");
  } else {
    panning = { x: ev.clientX, y: ev.clientY, panX: view.panX, panY: view.panY };
  }
});
video.addEventListener("mousemove", (ev) => {
  if (settings.control) {
    if (ev.timeStamp - lastMove < 16) return; // ~60/с
    lastMove = ev.timeStamp;
    ctrlMove(ev.clientX, ev.clientY);
  } else if (panning) {
    view.panX = panning.panX + (ev.clientX - panning.x);
    view.panY = panning.panY + (ev.clientY - panning.y);
    applyView();
  }
});
window.addEventListener("mouseup", (ev) => {
  if (settings.control) ctrlUp(ev.clientX, ev.clientY, ev.button === 2 ? "right" : "left");
  panning = null;
});
video.addEventListener(
  "wheel",
  (ev) => {
    ev.preventDefault();
    if (settings.control) {
      sendScroll(ev.deltaX, ev.deltaY); // режим управления: колесо → скролл хоста
    } else {
      zoomAt(ev.clientX, ev.clientY, ev.deltaY < 0 ? 1.15 : 1 / 1.15); // просмотр: зум
    }
  },
  { passive: false }
);

// Тач. Просмотр: 1 палец = пан, 2 пальца = пинч-зум вьюпорта.
// Управление: 1 палец = курсор/тап/drag (отложенно), 2 пальца = скролл хоста.
const tDist = (a, b) => Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
const tMid = (a, b) => ({ x: (a.clientX + b.clientX) / 2, y: (a.clientY + b.clientY) / 2 });
let pinch = null, touchPan = null;
let tCtrl = null; // отложенный 1-палец в режиме управления: {x,y,sent}
let tScroll = null; // центр 2-пальцевого скролла

video.addEventListener(
  "touchstart",
  (ev) => {
    ev.preventDefault();
    if (settings.control) {
      if (ev.touches.length >= 2) {
        // два пальца → скролл. Отменяем отложенный палец (если уже был drag — отпускаем).
        if (tCtrl && tCtrl.sent) sendMouseAt(tCtrl.x, tCtrl.y, "up", "left");
        tCtrl = null;
        tScroll = tMid(ev.touches[0], ev.touches[1]);
      } else {
        const t = ev.touches[0];
        tCtrl = { x: t.clientX, y: t.clientY, sent: false }; // down пошлём, когда поймём тап/drag
      }
    } else if (ev.touches.length === 2) {
      pinch = { dist: tDist(ev.touches[0], ev.touches[1]), mid: tMid(ev.touches[0], ev.touches[1]) };
      touchPan = null;
    } else {
      const t = ev.touches[0];
      touchPan = { x: t.clientX, y: t.clientY, panX: view.panX, panY: view.panY };
    }
  },
  { passive: false }
);
video.addEventListener(
  "touchmove",
  (ev) => {
    ev.preventDefault();
    if (settings.control) {
      if (tScroll && ev.touches.length >= 2) {
        const mid = tMid(ev.touches[0], ev.touches[1]);
        sendScroll(mid.x - tScroll.x, mid.y - tScroll.y);
        tScroll = mid;
      } else if (tCtrl) {
        const t = ev.touches[0];
        if (!t || ev.timeStamp - lastMove < 16) return;
        lastMove = ev.timeStamp;
        if (!tCtrl.sent) {
          if (Math.hypot(t.clientX - tCtrl.x, t.clientY - tCtrl.y) < settings.dragDeadzone) return; // дедзона
          sendMouseAt(tCtrl.x, tCtrl.y, "down", "left"); // начинаем drag с точки нажатия
          tCtrl.sent = true;
        }
        sendMouseAt(t.clientX, t.clientY, "move", "left");
      }
    } else if (ev.touches.length === 2 && pinch) {
      const dist = tDist(ev.touches[0], ev.touches[1]), mid = tMid(ev.touches[0], ev.touches[1]);
      view.panX += mid.x - pinch.mid.x; // следуем за центром щипка
      view.panY += mid.y - pinch.mid.y;
      zoomAt(mid.x, mid.y, dist / (pinch.dist || dist));
      pinch = { dist, mid };
    } else if (touchPan) {
      const t = ev.touches[0];
      view.panX = touchPan.panX + (t.clientX - touchPan.x);
      view.panY = touchPan.panY + (t.clientY - touchPan.y);
      applyView();
    }
  },
  { passive: false }
);
video.addEventListener(
  "touchend",
  (ev) => {
    if (settings.control) {
      if (tScroll && ev.touches.length < 2) {
        tScroll = null;
        tCtrl = null; // оставшийся палец не превращаем в клик
      } else if (tCtrl && ev.touches.length === 0) {
        if (!tCtrl.sent) {
          // тап → чистый клик в точке нажатия
          sendMouseAt(tCtrl.x, tCtrl.y, "down", "left");
          sendMouseAt(tCtrl.x, tCtrl.y, "up", "left");
        } else {
          const t = ev.changedTouches && ev.changedTouches[0];
          sendMouseAt(t ? t.clientX : tCtrl.x, t ? t.clientY : tCtrl.y, "up", "left");
        }
        tCtrl = null;
      }
    } else {
      if (ev.touches.length < 2) pinch = null;
      if (ev.touches.length === 0) touchPan = null;
    }
  },
  { passive: false }
);

// HUD-кнопки.
document.getElementById("btn-fullscreen").addEventListener("click", toggleFullscreen);
document.getElementById("btn-reset").addEventListener("click", resetView);
btnControl.addEventListener("click", () => setControl(!settings.control));
btnControl.classList.toggle("active", settings.control);
video.style.cursor = settings.control ? "crosshair" : "default";
updateAppsVisibility(); // показать ленту приложений (если не режим управления)

// Старт.
(async () => {
  await initSources();
  connect();
})();
