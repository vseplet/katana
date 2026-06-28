// term.js — общий терминал хоста, отрисованный xterm.js. I/O идёт по WebRTC
// DataChannel "term" (его создаёт сервер; app.js передаёт сюда через setChannel).
// Протокол: бинарь = сырые байты PTY (в обе стороны), текст (JSON) = управление
// ({t:"resize",cols,rows} | {t:"kill"}). PTY на сервере общий для всех зрителей
// и поднимается на первый resize.
(function () {
  let term = null, fit = null, dc = null;

  const body = () => document.getElementById("term-body");
  const visible = () => document.body.dataset.layout !== "desktop";

  function ensureTerm() {
    if (term || !window.Terminal) return;
    term = new window.Terminal({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      scrollback: 5000,
      theme: { background: "#0b0e14", foreground: "#cbd3e1" },
    });
    if (window.FitAddon && window.FitAddon.FitAddon) {
      fit = new window.FitAddon.FitAddon();
      term.loadAddon(fit);
    }
    term.open(body());
    window.__term = term; // для отладки из DevTools

    // Нажатия → stdin (бинарём).
    term.onData((d) => {
      if (dc && dc.readyState === "open") dc.send(new TextEncoder().encode(d));
    });
    // Изменение сетки → resize хосту (текстом). Сервер ставит размер PTY =
    // минимум по всем зрителям, поэтому шлём свой размер при любом изменении.
    term.onResize(({ cols, rows }) => sendResize(cols, rows));

    // Рефит при любом изменении размера контейнера (сплит, поворот, ресайз окна).
    if (window.ResizeObserver) {
      let raf = 0;
      const ro = new ResizeObserver(() => {
        if (raf) return;
        raf = requestAnimationFrame(() => { raf = 0; doFit(); });
      });
      ro.observe(body());
    }
    window.addEventListener("resize", () => { if (visible()) doFit(); });
  }

  // doFit подгоняет сетку под контейнер. Никогда не фитим скрытую панель (0×0):
  // FitAddon тогда вернёт NaN и испортит геометрию (мышь/координаты ломаются).
  function doFit() {
    if (!fit) return;
    const el = body();
    if (!el || el.offsetWidth === 0 || el.offsetHeight === 0) return;
    try { fit.fit(); } catch (_) {}
  }

  // sendResize шлёт текущую сетку хосту. onResize срабатывает только при ИЗМЕНЕНИИ
  // cols/rows, поэтому при первом показе/реконнекте (та же сетка) пушим явно —
  // именно первый resize поднимает PTY на сервере и вызывает реплей экрана.
  function sendResize(cols, rows) {
    if (!term || !dc || dc.readyState !== "open") return;
    dc.send(JSON.stringify({ t: "resize", cols: cols || term.cols, rows: rows || term.rows }));
  }

  function setChannel(channel) {
    dc = channel;
    dc.binaryType = "arraybuffer";
    dc.onmessage = (e) => {
      if (!term) return;
      if (typeof e.data === "string") term.write(e.data);
      else term.write(new Uint8Array(e.data));
    };
    dc.onopen = () => { if (visible()) { ensureTerm(); doFit(); sendResize(); term.focus(); } };
    // Канал мог открыться раньше, чем мы его получили.
    if (dc.readyState === "open" && visible()) { ensureTerm(); doFit(); sendResize(); }
  }

  // show вызывается app.js при переходе в split/terminal: создаём терминал,
  // подгоняем и пушим размер (поднимет PTY и попросит реплей экрана).
  function show() {
    ensureTerm();
    if (!term) return;
    // На следующем кадре — когда панель уже получила размеры из CSS-раскладки.
    requestAnimationFrame(() => {
      doFit();
      sendResize();
      term.focus();
      try { term.refresh(0, term.rows - 1); } catch (_) {}
    });
  }

  function hide() { /* PTY общий и живёт на сервере — ничего не рвём */ }

  // refit — подогнать сетку под изменившийся размер панели (после ресайза/сплита).
  function refit() {
    if (!term) return;
    requestAnimationFrame(() => { doFit(); sendResize(); });
  }

  window.katanaTerm = { setChannel, show, hide, refit, kill() {
    if (dc && dc.readyState === "open") dc.send(JSON.stringify({ t: "kill" }));
  } };
})();
