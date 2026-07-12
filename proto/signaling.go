package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/vseplet/katana/proto/capture"
)

// signalMessage — формат сообщений сигналинга (JSON over WS), см. §4 ТЗ.
// Типы "config"/"mouse" — расширения поверх ТЗ.
type signalMessage struct {
	Type      string                   `json:"type"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Config    *configMsg               `json:"config,omitempty"`
	Mouse     *mouseMsg                `json:"mouse,omitempty"`
	Scroll    *scrollMsg               `json:"scroll,omitempty"`
	Dir       string                   `json:"dir,omitempty"` // zoom: "in" | "out"
	Key       *keyMsg                  `json:"key,omitempty"`
	Text      string                   `json:"text,omitempty"` // для "type": набор текста
	Pad       *gamepadMsg              `json:"pad,omitempty"`  // для "gamepad"
	// Vid — идентификатор зрителя (viewer/peer id). Несколько зрителей делят один
	// WS хоста через брокер; vid адресует сигналинг конкретному зрителю. Зритель
	// генерирует его при подключении и проставляет во все свои сообщения; хост
	// возвращает тот же vid в offer/candidate/state, чтобы зритель отфильтровал
	// чужие. (Отдельно от app-PID ниже — у activate своё числовое поле "pid".)
	Vid string `json:"vid,omitempty"`
	// Параметры для "hello" — зритель выбирает кодек и звук при подключении
	// (как кодек/аудио меняются: новый трек = нужен новый PeerConnection).
	Codec string `json:"codec,omitempty"`
	Audio *bool  `json:"audio,omitempty"`
	// Источники захвата: запрос ("sources") и ответ хоста (Sources); активация
	// приложения ("activate" + PID). Раньше это было HTTP-API хоста.
	Sources *capture.Sources `json:"sources,omitempty"`
	PID     int              `json:"pid,omitempty"`
	// Инфо о хосте (для "hostinfo" — заголовок вкладки зрителя).
	OS       string `json:"os,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Ffmpeg   bool   `json:"ffmpeg,omitempty"` // есть ли ffmpeg на хосте (для доступности VP8)
	// Возможности хоста в этом окружении (для "hostinfo") — вьювер адаптируется.
	Video    bool `json:"video,omitempty"`
	AudioCap bool `json:"audioCap,omitempty"`
	Input    bool `json:"input,omitempty"`
	Terminal bool `json:"terminal,omitempty"`
	Gamepad  bool `json:"gamepad,omitempty"`
	// MouseCapture — хост умеет сырой relative-ввод для захвата мыши в играх
	// (action "rel" + сообщение "capture"). Вьювер по этому флагу включает режим
	// захвата (Pointer Lock). См. docs/mouse-capture.md.
	MouseCapture bool `json:"mouseCapture,omitempty"`
	// On — вкл/выкл для сообщения "capture" (курсор захвачен игрой на вьювере).
	On bool `json:"on,omitempty"`
	// T — timestamp (performance.now() браузера) для ping/pong RTT-замера.
	T float64 `json:"t,omitempty"`
	// Broker → host для TUI: "sessioninfo" (владелец+план) и "presence" (зрители).
	Owner   string        `json:"owner,omitempty"`
	Plan    string        `json:"plan,omitempty"`
	Viewers []viewerCount `json:"viewers,omitempty"`
	// Присутствие курсоров зрителей ("multiplayer cursors"):
	//   "vcursor" (зритель → хост): позиция СВОЕГО указателя над видео (Mouse.X/Y,
	//   норм. [0,1]); Gone=true — указатель ушёл с видео. НЕ двигает OS-курсор хоста,
	//   хост лишь складывает по vid.
	//   "peers" (хост → зритель): позиции указателей ОСТАЛЬНЫХ зрителей (Cursors) —
	//   каждый рисует их поверх видео. Рассылается низкочастотно, вьювер сглаживает
	//   движение интерполяцией.
	Gone    bool         `json:"gone,omitempty"`
	Cursors []peerCursor `json:"cursors,omitempty"`
	// Name — ник зрителя для подписи его курсора ("vcursorname", шлётся один раз).
	Name string `json:"name,omitempty"`
}

// peerCursor — позиция указателя одного зрителя для рисования у остальных.
// X/Y нормализованы [0,1] к содержимому видео (одинаковы у всех зрителей).
type peerCursor struct {
	ID    string  `json:"id"`   // vid зрителя (стабильный ключ для интерполяции)
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Color string  `json:"color"`          // цвет иконки (детерминированно из vid)
	Name  string  `json:"name,omitempty"` // ник зрителя для подписи (если прислал)
}

// viewerCount — один пользователь-зритель и число его открытых вкладок (views).
type viewerCount struct {
	Name  string `json:"name"`
	Views int    `json:"views"`
}

// osLabel — человекочитаемое имя ОС хоста.
func osLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

// scrollMsg — событие прокрутки от браузера (в «кликах» колеса).
type scrollMsg struct {
	Dx int `json:"dx"`
	Dy int `json:"dy"`
}

// keyMsg — нажатие клавиши с модификаторами (спец-клавиши и шорткаты).
type keyMsg struct {
	Key  string   `json:"key"`
	Mods []string `json:"mods,omitempty"`
}

// gamepadMsg — событие геймпада от браузера (Gamepad API).
// Kind="btn": кнопка (Idx — индекс, Down — нажата, Val — аналоговое значение 0..1).
// Kind="axis": ось (Idx — индекс, Val — значение -1..1).
type gamepadMsg struct {
	Kind string  `json:"kind"` // "btn" | "axis"
	Idx  int     `json:"idx"`
	Down bool    `json:"down"`
	Val  float64 `json:"val"`
}

// mouseMsg — событие мыши от браузера. X/Y — нормализованные [0,1] координаты
// относительно содержимого видео.
type mouseMsg struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Action string  `json:"action"` // move|down|up | moverel|dragrel|click|press|release | rel
	Button string  `json:"button"` // left | right
	// Относительное движение. Dx/Dy — пиксели хоста.
	//   moverel/dragrel — десктопный трекпад мобилы (→ абсолютное позиционирование);
	//   rel             — сырой захват мыши для игр (→ relative uinput-устройство).
	Dx int `json:"dx,omitempty"`
	Dy int `json:"dy,omitempty"`
}

// configMsg — настройки захвата, присылаемые браузером. Указатели, чтобы
// отличать «не задано» от нуля; незаданные поля сохраняют текущее значение.
type configMsg struct {
	SourceKind  *string `json:"sourceKind,omitempty"` // screen | window | app
	SourceID    *int    `json:"sourceId,omitempty"`   // windowID / pid (для window/app)
	Screen      *int    `json:"screen,omitempty"`     // индекс avfoundation (для screen)
	Width       *int    `json:"width,omitempty"`
	FPS         *int    `json:"fps,omitempty"`
	BitrateKbps *int    `json:"bitrateKbps,omitempty"`
	AutoBitrate *bool   `json:"autoBitrate,omitempty"` // адаптировать битрейт под сеть
	Threads     *int    `json:"threads,omitempty"`
	DropLate    *bool   `json:"dropLate,omitempty"`
	Cursor      *bool   `json:"cursor,omitempty"`
}

// apply накладывает настройки на базовые опции с клампингом разумных границ.
// Битрейт приходит числом (kbps) — не строкой, чтобы не пробрасывать
// произвольный текст в аргументы ffmpeg.
func (c *configMsg) apply(base capture.Options) capture.Options {
	o := base
	if c.SourceKind != nil {
		o.SourceKind = *c.SourceKind
	}
	if c.SourceID != nil {
		o.SourceID = *c.SourceID
	}
	if c.Screen != nil {
		o.ScreenIndex = clamp(*c.Screen, 0, 64)
	}
	if c.Width != nil {
		if *c.Width == 0 {
			o.Width = 0 // нативное разрешение (без даунскейла)
		} else {
			o.Width = clamp(*c.Width, 320, 7680)
		}
	}
	if c.FPS != nil {
		o.FPS = clamp(*c.FPS, 1, 60)
	}
	if c.BitrateKbps != nil {
		o.Bitrate = fmt.Sprintf("%dk", clamp(*c.BitrateKbps, 100, 20000))
	}
	if c.Threads != nil {
		o.Threads = clamp(*c.Threads, 0, 16)
	}
	if c.DropLate != nil {
		o.DropLate = *c.DropLate
	}
	if c.Cursor != nil {
		o.Cursor = *c.Cursor
	}
	return o
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// optsToState строит "state"-сообщение с ТЕКУЩИМИ настройками трансляции для
// зрителя — чтобы его UI синхронизировался с уже идущим потоком, а не показывал
// собственные дефолты (и не отправлял их обратно).
func optsToState(o capture.Options) signalMessage {
	codec := "vp8"
	if o.Codec == capture.CodecH264 {
		codec = "h264"
	}
	audio := o.Audio
	kind := o.SourceKind
	sid := o.SourceID
	screen := o.ScreenIndex
	width := o.Width
	fps := o.FPS
	cursor := o.Cursor
	return signalMessage{
		Type:  "state",
		Codec: codec,
		Audio: &audio,
		Config: &configMsg{
			SourceKind: &kind,
			SourceID:   &sid,
			Screen:     &screen,
			Width:      &width,
			FPS:        &fps,
			Cursor:     &cursor,
		},
	}
}

// runBrokerHost подключается исходящим WS к рандеву-брокеру как host и ведёт
// сессию через него (режим: katana --id=<uuid>). WS живёт постоянно, пока хост
// запущен; несколько зрителей подключаются и отключаются через него.
func runBrokerHost(ctx context.Context, brokerURL, sessionID string, enc capture.CaptureEncoder, opts capture.Options) {
	wsURL := fmt.Sprintf("%s?session=%s&role=host",
		strings.TrimRight(brokerURL, "/"), url.QueryEscape(sessionID))
	loadICEServers(ctx, brokerURL) // до цикла: список нужен уже первому зрителю
	for ctx.Err() == nil {
		log.Printf("broker: connecting to %s (session %s)", brokerURL, sessionID)
		uiStatus("connecting to broker…")
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			log.Printf("broker: dial: %v (retry in 3s)", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		log.Printf("broker: connected, waiting for viewers")
		uiStatus("connected · waiting for viewers")
		reject, reason := serveHub(ctx, conn, enc, opts, "broker:"+sessionID)
		if reject {
			// Брокер отклонил хост (лимит free-плана / неизвестная сессия). Это не
			// сетевой сбой — переподключаться бессмысленно. Печатаем причину и выходим.
			if reason == "" {
				reason = "rejected by broker"
			}
			log.Printf("host: cannot start — %s", reason)
			log.Printf("host: manage or upgrade at %s", dashboardURL(brokerURL))
			uiStatus("stopped — " + reason)
			return
		}
		if ctx.Err() == nil {
			log.Printf("broker: connection lost, reconnecting")
			uiStatus("connection lost · reconnecting…")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// dashboardURL выводит адрес дашборда (для апгрейда) из URL брокера:
// wss://host/rtc → https://host/sessions.
func dashboardURL(brokerURL string) string {
	return originFromBroker(brokerURL) + "/sessions"
}

// viewerURL — ссылка зрителя (для QR в TUI): wss://host/rtc → https://host/v/<session>.
func viewerURL(brokerURL, session string) string {
	return originFromBroker(brokerURL) + "/v/" + session
}

func originFromBroker(brokerURL string) string {
	u := strings.TrimSuffix(strings.TrimRight(brokerURL, "/"), "/rtc")
	u = strings.Replace(u, "wss://", "https://", 1)
	u = strings.Replace(u, "ws://", "http://", 1)
	return u
}

// hostICEServers — ICE-серверы для PeerConnection зрителей. Тянем из /api/ice
// (SaaS — источник правды, туда же добавится TURN без пересборки хоста). Фолбэк
// — публичный Google STUN. Заполняется один раз в loadICEServers() до цикла
// подключения; читается в buildLocked (гонки нет — загрузка завершается раньше).
var hostICEServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
}

// loadICEServers запрашивает список ICE у брокера. Формат ответа:
// {"iceServers":[{"urls":["stun:..."|"..."],"username":"?","credential":"?"}]}.
// urls допускается и строкой, и массивом (как в спецификации RTCIceServer).
func loadICEServers(ctx context.Context, brokerURL string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, originFromBroker(brokerURL)+"/api/ice", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return // сеть — остаёмся на фолбэке
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var body struct {
		IceServers []struct {
			URLs       json.RawMessage `json:"urls"`
			Username   string          `json:"username"`
			Credential string          `json:"credential"`
		} `json:"iceServers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return
	}
	out := make([]webrtc.ICEServer, 0, len(body.IceServers))
	for _, s := range body.IceServers {
		var urls []string
		if err := json.Unmarshal(s.URLs, &urls); err != nil {
			var one string
			if json.Unmarshal(s.URLs, &one) != nil || one == "" {
				continue
			}
			urls = []string{one}
		}
		if len(urls) == 0 {
			continue
		}
		out = append(out, webrtc.ICEServer{URLs: urls, Username: s.Username, Credential: s.Credential})
	}
	if len(out) > 0 {
		hostICEServers = out
		n := 0
		for _, s := range out {
			n += len(s.URLs)
		}
		log.Printf("ice: loaded %d server(s), %d URL(s) from broker", len(out), n)
	}
}

// captureGrace — сколько держать захват живым после ухода последнего зрителя,
// чтобы краткий реконнект (особенно на мобильной сети) не перезапускал SCK.
const captureGrace = 10 * time.Second

// hub — хост-узел: одно WS-соединение с брокером, ОДИН общий захват и набор
// зрителей (peer). Видео/аудио-треки общие: pion раздаёт WriteSample во все
// привязанные PeerConnection, поэтому экран кодируется один раз на всех.
//
// Настройки трансляции (кодек/источник/разрешение/аудио) — «липкие»: их задаёт
// первый зритель, дальше они живут в hub и применяются ко всем; новый зритель
// синхронизируется с ними, а не навязывает свои.
type hub struct {
	ctx  context.Context
	enc  capture.CaptureEncoder
	base capture.Options
	ws   *websocket.Conn
	cnl  context.CancelFunc

	writeMu sync.Mutex // сериализует запись в общий WS

	reject       bool   // брокер отклонил хост (1008) — НЕ переподключаться
	rejectReason string // причина отклонения (текст из close-фрейма)

	mu         sync.Mutex // защищает всё ниже
	configured bool       // задавались ли уже настройки трансляции
	curOpts    capture.Options
	str        *streamer
	vtrack     *webrtc.TrackLocalStaticSample
	atrack     *webrtc.TrackLocalStaticSample
	peers      map[string]*peer
	stopTimer  *time.Timer // отложенная остановка захвата при 0 зрителей (grace)
	lastKeyReq time.Time   // дебаунс форса keyframe по PLI (keyframe дорог)
	keySuppressed int      // PLI, подавленные дебаунсом с прошлого форса (диагностика шторма)
	lastStormCut  time.Time // дебаунс принудительного среза битрейта при PLI-шторме
	autoBitrate bool       // адаптивный битрейт включён (по запросу зрителя)
	curBitrate  int        // текущий битрейт энкодера, kbps (для адаптации)
	maxBitrate  int        // потолок адаптации = целевой битрейт настроек, kbps
	lastBrAdj   time.Time  // дебаунс шага адаптации битрейта
	cleanTicks  int        // подряд тиков без потерь (для осторожного подъёма)
	lastLossLog time.Time  // троттлинг диагностического лога RR-потерь

	srcMu sync.Mutex   // защищает геометрию источника (для координат мыши)
	rect  capture.Rect // глобальный прямоугольник общего источника
}

// peer — один зритель: своё PeerConnection поверх общих треков хаба и свои
// data-каналы (input/term). Видео/аудио-треки НЕ свои — общие из hub.
type peer struct {
	h           *hub
	pid         string
	pc          *webrtc.PeerConnection
	videoSender *webrtc.RTPSender
	audioSender *webrtc.RTPSender

	gotAnswer  bool                          // получили ли answer (рукопожатие завершено) — под hub.mu
	pendingICE []webrtc.ICECandidateInit     // ICE до установки remote description — под hub.mu

	btnDown string // зажатая кнопка мыши ("" если нет) — для drag
	dragged bool   // были ли move с зажатой кнопкой (отличить drag от клика)
	// captureActive — вьювер захватил указатель (игра, Pointer Lock). Пока true:
	// движение шлётся как action "rel" (→ relative-устройство), cursorpos не шлём.
	captureActive bool
	// kbState — текущие нажатые HID-коды клавиш от этого зрителя (state-based ввод).
	kbState map[uint8]bool

	inputDC          *webrtc.DataChannel // канал ввода (для отчёта позиции курсора)
	lastCursorReport time.Time           // троттлинг cursorpos
	cursorTimer      *time.Timer         // трейлинг: дослать финальную позицию после остановки

	// Позиция СВОЕГО указателя этого зрителя над видео (norm. [0,1]) — для рисования
	// у остальных зрителей. Обновляется из "vcursor", НЕ двигает OS-курсор. Под hub.mu.
	vcX, vcY float64
	vcTS     time.Time // время последнего vcursor (для отсева протухших)
	vcActive bool      // указатель сейчас над видео (Gone=false)
	vcName   string    // ник для подписи курсора (из "vcursorname", один раз)
	hadPeers bool      // в прошлую рассылку этому зрителю ушёл непустой "peers" (для гейта)
}

// serveHub ведёт хост-узел поверх готового WS-соединения с брокером.
// serveHub ведёт сессию хоста поверх WS. Возвращает (reject, reason): reject=true
// значит брокер отклонил хост (1008) и переподключаться НЕ нужно.
func serveHub(parent context.Context, conn *websocket.Conn, enc capture.CaptureEncoder, connOpts capture.Options, label string) (bool, string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	h := &hub{
		ctx:   ctx,
		enc:   enc,
		base:  connOpts,
		ws:    conn,
		cnl:   cancel,
		peers: map[string]*peer{},
	}
	defer func() {
		h.mu.Lock()
		if h.stopTimer != nil {
			h.stopTimer.Stop()
			h.stopTimer = nil
		}
		for pid, p := range h.peers {
			p.closePC()
			delete(h.peers, pid)
		}
		if h.str != nil {
			h.str.stop()
			h.str = nil
		}
		h.mu.Unlock()
		_ = conn.CloseNow()
		log.Printf("signaling: host session ended (%s)", label)
	}()

	// Keepalive: периодически пингуем брокер. Если pong не пришёл (сокет мёртв —
	// например Mac уснул и проснулся, TCP заморожен и обрыв сам не детектится),
	// рвём контекст → readLoop выходит → runBrokerHost переподключается. Без
	// этого хост после сна висел бы на мёртвом Read, и зрители не могли подключиться.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 7*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("broker: keepalive ping failed: %v — reconnecting", err)
						cancel()
					}
					return
				}
			}
		}
	}()

	// Рассылка курсоров зрителей друг другу (тип "peers"). Низкочастотно (10/с) —
	// вьювер сглаживает движение интерполяцией, поэтому частить незачем.
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.broadcastCursors()
			}
		}
	}()

	h.readLoop()
	return h.reject, h.rejectReason
}

// cursorPalette — различимые цвета для курсоров зрителей (детерминированно по vid).
var cursorPalette = []string{
	"#e5484d", "#4589ff", "#30a46c", "#f76b15",
	"#8e4ec6", "#e2a336", "#e93d82", "#00a2c7",
}

// cursorColor детерминированно выбирает цвет из палитры по id (FNV-1a).
func cursorColor(id string) string {
	var hsh uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		hsh = (hsh ^ uint32(id[i])) * 16777619
	}
	return cursorPalette[hsh%uint32(len(cursorPalette))]
}

// cursorState — снимок присутствия одного зрителя для расчёта рассылки.
type cursorState struct {
	id       string
	active   bool // указатель над видео (vcActive)
	ts       time.Time
	x, y     float64
	name     string
	hadPeers bool // в прошлую рассылку этому зрителю ушёл непустой список
}

// planCursorBroadcast — ЧИСТОЕ решение «кому что слать» (без сети, тестируемо).
// Каждому зрителю — курсоры ОСТАЛЬНЫХ активных и не протухших (>1с). В карте send
// присутствуют только те, кому реально надо слать: пустой список шлём один раз
// после того, как курсоры пропали (гейт hadPeers), чтобы вьювер убрал иконки, но
// не спамить пустотой в простое. had — новое значение hadPeers по id.
func planCursorBroadcast(states []cursorState, now time.Time) (send map[string][]peerCursor, had map[string]bool) {
	live := make([]peerCursor, 0, len(states))
	for _, s := range states {
		if s.active && now.Sub(s.ts) <= time.Second {
			live = append(live, peerCursor{ID: s.id, X: s.x, Y: s.y, Color: cursorColor(s.id), Name: s.name})
		}
	}
	send = map[string][]peerCursor{}
	had = map[string]bool{}
	for _, dst := range states {
		cs := make([]peerCursor, 0, len(live))
		for _, c := range live {
			if c.ID != dst.id {
				cs = append(cs, c)
			}
		}
		had[dst.id] = len(cs) > 0
		if len(cs) == 0 && !dst.hadPeers {
			continue // нечего слать и в прошлый раз тоже — молчим
		}
		send[dst.id] = cs
	}
	return send, had
}

// broadcastCursors рассылает каждому зрителю позиции указателей ОСТАЛЬНЫХ зрителей
// (тип "peers"). Логику «кому что» считает planCursorBroadcast; здесь — снимок под
// локом и собственно отправка по inputDC.
func (h *hub) broadcastCursors() {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	states := make([]cursorState, 0, len(h.peers))
	for pid, p := range h.peers {
		if p.inputDC == nil {
			continue // без канала слать некуда и учитывать в рассылке незачем
		}
		states = append(states, cursorState{
			id: pid, active: p.vcActive, ts: p.vcTS, x: p.vcX, y: p.vcY, name: p.vcName, hadPeers: p.hadPeers,
		})
	}
	send, had := planCursorBroadcast(states, now)
	for pid, p := range h.peers {
		if _, ok := had[pid]; ok {
			p.hadPeers = had[pid]
		}
		cs, ok := send[pid]
		if !ok || p.inputDC == nil {
			continue
		}
		if b, err := json.Marshal(signalMessage{Type: "peers", Cursors: cs}); err == nil {
			_ = p.inputDC.SendText(string(b))
		}
	}
}

// send сериализует сообщение (с проставленным pid) и пишет его в общий WS.
func (h *hub) send(msg signalMessage, pid string) {
	msg.Vid = pid
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("signaling: marshal %s: %v", msg.Type, err)
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if err := h.ws.Write(h.ctx, websocket.MessageText, data); err != nil {
		if h.ctx.Err() == nil {
			log.Printf("signaling: write %s: %v", msg.Type, err)
		}
	}
}

// readLoop читает сигналинг всех зрителей с общего WS и роутит по pid.
func (h *hub) readLoop() {
	for {
		_, data, err := h.ws.Read(h.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
				// Брокер отклонил хост (неизвестная сессия / лимит free-плана).
				// Запоминаем причину и НЕ переподключаемся — это не сетевой сбой.
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					h.rejectReason = ce.Reason
				}
				h.reject = true
			} else if h.ctx.Err() == nil && websocket.CloseStatus(err) == -1 && !errors.Is(err, context.Canceled) {
				log.Printf("signaling: ws read: %v", err)
			}
			return
		}

		var msg signalMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("signaling: unmarshal: %v", err)
			continue
		}
		pid := msg.Vid
		if pid == "" {
			pid = "default" // зритель без vid (старый клиент) — единственный «default»
		}

		switch msg.Type {
		case "hello":
			h.onHello(pid, msg)
		case "renegotiate":
			h.onRenegotiate(msg)
		case "answer":
			if p := h.peer(pid); p != nil && p.pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
				// Применяем answer только если реально ждём его (есть локальный
				// offer). Брокер иногда доставляет answer дублями — без этой
				// проверки повтор летит в уже установленное (stable) соединение
				// и сыплет InvalidModificationError.
				ans := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP}
				if err := p.pc.SetRemoteDescription(ans); err != nil {
					log.Printf("signaling: set remote description: %v", err)
				} else {
					// Рукопожатие завершено: больше не пересылаем offer и сливаем
					// ICE-кандидаты, накопленные до установки remote description.
					h.mu.Lock()
					p.gotAnswer = true
					pending := p.pendingICE
					p.pendingICE = nil
					h.mu.Unlock()
					for _, c := range pending {
						if err := p.pc.AddICECandidate(c); err != nil {
							log.Printf("signaling: add buffered ice: %v", err)
						}
					}
				}
			}
		case "candidate":
			if p := h.peer(pid); p != nil && msg.Candidate != nil {
				// До answer remote description ещё не установлен — буферизуем, иначе
				// pion вернёт "remote description is not set" и кандидат потеряется
				// (на мобильной сети это напрямую бьёт по установлению ICE).
				h.mu.Lock()
				if !p.gotAnswer {
					p.pendingICE = append(p.pendingICE, *msg.Candidate)
					h.mu.Unlock()
				} else {
					h.mu.Unlock()
					if err := p.pc.AddICECandidate(*msg.Candidate); err != nil {
						log.Printf("signaling: add ice candidate: %v", err)
					}
				}
			}
		case "config":
			h.onConfig(msg)
		case "keyframe":
			// Зритель просит свежий IDR (рассыпалась картинка / зашёл в середину).
			// При почти бесконечном GOP это, наряду с RTCP PLI, главный источник
			// кейфреймов — раньше сообщение роняли в default как unknown.
			h.requestKeyframe()
		case "mouse", "scroll", "cursor", "key", "type":
			if p := h.peer(pid); p != nil {
				p.dispatchInput(&msg) // фолбэк, если DataChannel ещё не открыт
			}
		case "sessioninfo":
			// От брокера: владелец сессии + уровень подписки — для TUI.
			uiOwner(msg.Owner, msg.Plan)
		case "presence":
			// От брокера: список зрителей (имя + число вкладок) — для TUI.
			uiViewerList(msg.Viewers)
		case "diag":
			// Диаг-снимок связи от зрителя — игнорируем (раньше логировали).
		default:
			log.Printf("signaling: unknown message type %q", msg.Type)
		}
	}
}

// peer возвращает зрителя по pid (или nil).
func (h *hub) peer(pid string) *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers[pid]
}

// onHello подключает зрителя: при первом — задаёт настройки трансляции и
// стартует общий захват; при последующих — берёт текущие (липкие) настройки.
func (h *hub) onHello(pid string, msg signalMessage) {
	h.mu.Lock()
	// Пришёл зритель — отменяем отложенную остановку захвата (grace). Если захват
	// ещё жив, ниже он переиспользуется (str != nil), без перезапуска.
	if h.stopTimer != nil {
		h.stopTimer.Stop()
		h.stopTimer = nil
	}
	// Уже есть peer с таким pid?
	if p := h.peers[pid]; p != nil {
		switch p.pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected:
			// Реальное переподключение этого зрителя (reload) — пересоздаём.
			log.Printf("broker: viewer %s reconnect (%s) — recreating peer", pid, p.pc.ConnectionState())
			p.closePC()
			delete(h.peers, pid)
		default:
			// Рукопожатие ещё идёт. На Deno Deploy BroadcastChannel доставляет
			// только уже подписанным изолятам, поэтому наш offer мог потеряться —
			// пока зритель не прислал answer, переотправляем offer на каждый
			// повторный hello, иначе зритель вечно висит на "waiting for host".
			if !p.gotAnswer {
				off := p.pc.LocalDescription()
				h.mu.Unlock()
				if off != nil {
					h.send(signalMessage{Type: "offer", SDP: off.SDP}, pid)
				}
				return
			}
			h.mu.Unlock()
			return // answer уже получен, идёт ICE — дубль hello игнорируем
		}
	}

	// Первый за всё время зритель задаёт настройки трансляции.
	if !h.configured {
		h.curOpts = h.buildOpts(msg)
		h.configured = true
	}
	h.applyAutoBitrateLocked(msg)
	// Захват ещё не идёт (первый зритель сейчас, либо все уходили и вернулись) —
	// поднимаем его с текущими (липкими) настройками.
	if h.str == nil {
		if err := h.startCaptureLocked(h.curOpts); err != nil {
			h.mu.Unlock()
			log.Printf("signaling: start capture: %v", err)
			return
		}
	}

	p := &peer{h: h, pid: pid}
	if err := p.buildLocked(); err != nil {
		h.mu.Unlock()
		log.Printf("signaling: build peer %s: %v", pid, err)
		return
	}
	h.peers[pid] = p
	opts := h.curOpts
	nviewers := len(h.peers)
	h.mu.Unlock()
	uiViewers(nviewers)
	uiStatus("live · streaming")

	p.offer()
	// Сообщаем зрителю текущие настройки трансляции (его UI синхронизируется,
	// а не сбрасывает поток под свои дефолты).
	h.send(optsToState(opts), pid)
}

// startCaptureLocked создаёт общие видео/аудио-треки и стартует ОДИН захват под
// ними. Вызывать под h.mu.
func (h *hub) startCaptureLocked(opts capture.Options) error {
	vtrack, atrack, err := newSharedTracks(opts)
	if err != nil {
		return err
	}
	str := newStreamer(h.ctx, h.enc, vtrack, atrack)
	if err := str.reconfigure(opts); err != nil {
		return err
	}
	h.vtrack = vtrack
	h.atrack = atrack
	h.str = str
	h.setSource(opts.SourceKind, opts.SourceID)
	return nil
}

// stopCaptureLocked останавливает общий захват (когда не осталось зрителей).
// Вызывать под h.mu.
func (h *hub) stopCaptureLocked() {
	if h.str != nil {
		h.str.stop()
		h.str = nil
	}
	h.vtrack = nil
	h.atrack = nil
}

// removePeer закрывает зрителя и, если он был последним, гасит общий захват.
// Настройки (curOpts) сохраняются — следующий зритель подхватит их.
func (h *hub) removePeer(pid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.peers[pid]
	if p == nil {
		return
	}
	p.releaseKbState()
	p.closePC()
	delete(h.peers, pid)
	log.Printf("broker: viewer %s left (%d remaining)", pid, len(h.peers))
	uiViewers(len(h.peers))
	if len(h.peers) == 0 {
		uiStatus("connected · waiting for viewers")
	}
	if len(h.peers) == 0 {
		// Захват не глушим сразу: на мобильной сети зритель часто рвётся и
		// возвращается за пару секунд. Даём grace-период — если за него никто
		// не подключился, тогда останавливаем. Реконнект в пределах grace
		// переиспользует живой захват без перезапуска SCK/VideoToolbox.
		if h.stopTimer != nil {
			h.stopTimer.Stop()
		}
		h.stopTimer = time.AfterFunc(captureGrace, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.peers) == 0 && h.str != nil {
				h.stopCaptureLocked()
				log.Printf("broker: no viewers for %s — capture stopped (settings kept)", captureGrace)
			}
		})
	}
}

// onConfig применяет настройки захвата (источник/разрешение/fps/битрейт/курсор)
// к ОБЩЕМУ потоку — изменение видят все зрители. Смена кодека/аудио идёт через
// "renegotiate" (требует новых треков). Без SDP-ренеготиации.
func (h *hub) onConfig(msg signalMessage) {
	if msg.Config == nil {
		return
	}
	h.mu.Lock()
	if h.str == nil {
		h.mu.Unlock()
		return
	}
	newOpts := msg.Config.apply(h.curOpts)
	h.curOpts = newOpts
	h.applyAutoBitrateLocked(msg)
	str := h.str
	h.mu.Unlock()

	// Настройки общие — синхронизируем панель у ВСЕХ зрителей (не только у того,
	// кто менял), чтобы их UI совпадал с новым потоком.
	h.broadcastState()

	// Перезапуск ffmpeg/SCK может занять ~секунду — в фоне, чтобы не блокировать
	// чтение WS (и приём ICE).
	go func() {
		if err := str.reconfigure(newOpts); err != nil {
			log.Printf("signaling: reconfigure: %v", err)
		}
		h.mu.Lock()
		h.setSource(newOpts.SourceKind, newOpts.SourceID)
		h.mu.Unlock()
	}()
}

// onRenegotiate меняет кодек/аудио ОБЩЕГО потока: пересоздаёт общие треки и
// шлёт оффер ренеготиации каждому зрителю — на тех же PeerConnection (без
// разрыва). Видео-трек всегда новый (новый SSRC → Chrome поднимает свежий
// декодер; H264 иначе виснет), Opus добавляется/снимается по opts.Audio.
func (h *hub) onRenegotiate(msg signalMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.str == nil {
		return
	}
	opts := h.applyHello(h.curOpts, msg)
	h.curOpts = opts
	h.applyAutoBitrateLocked(msg)

	h.str.stop() // остановить захват перед подменой треков

	vtrack, atrack, err := newSharedTracks(opts)
	if err != nil {
		log.Printf("reneg: new tracks: %v", err)
		h.cnl()
		return
	}

	st := optsToState(opts)
	for pid, p := range h.peers {
		if p.videoSender != nil {
			_ = p.pc.RemoveTrack(p.videoSender)
			p.videoSender = nil
		}
		if p.audioSender != nil {
			_ = p.pc.RemoveTrack(p.audioSender)
			p.audioSender = nil
		}
		vsender, err := p.pc.AddTrack(vtrack)
		if err != nil {
			log.Printf("reneg: add video track (%s): %v", pid, err)
			continue
		}
		go readRTCP(vsender, h.requestKeyframe, h.onLoss)
		p.videoSender = vsender
		if atrack != nil {
			asender, err := p.pc.AddTrack(atrack)
			if err != nil {
				log.Printf("reneg: add audio track (%s): %v", pid, err)
			} else {
				go readRTCP(asender, nil, nil)
				p.audioSender = asender
			}
		}
		p.offer()
		h.send(st, pid) // синхронизируем панель зрителя с новым кодеком/аудио
	}

	h.vtrack = vtrack
	h.atrack = atrack
	h.str.setTracks(vtrack, atrack)
	log.Printf("broker: renegotiating all viewers (codec=%s audio=%v)", opts.Codec, opts.Audio)

	// Рестарт захвата под новые треки — в фоне (~1с), чтобы не держать h.mu.
	str := h.str
	go func() {
		if err := str.reconfigure(opts); err != nil {
			log.Printf("reneg: reconfigure: %v", err)
		}
		h.mu.Lock()
		h.setSource(opts.SourceKind, opts.SourceID)
		h.mu.Unlock()
	}()
}

// broadcastState рассылает текущие настройки трансляции всем зрителям, чтобы их
// панели совпадали с общим потоком после изменения настроек кем-то одним.
func (h *hub) broadcastState() {
	h.mu.Lock()
	st := optsToState(h.curOpts)
	pids := make([]string, 0, len(h.peers))
	for pid := range h.peers {
		pids = append(pids, pid)
	}
	h.mu.Unlock()
	for _, pid := range pids {
		h.send(st, pid)
	}
}

// requestKeyframe форсит keyframe общего энкодера в ответ на PLI зрителя —
// зритель дропает накопленный буфер и прыгает к свежему кадру (догоняет live).
//
// Дебаунс 1с — защита от IDR-шторма: на WAN (~300мс) зритель шлёт следующий PLI
// раньше, чем долетит и декодируется IDR от предыдущего; при коротком дебаунсе
// каждый PLI рождает новый жирный IDR в уже забитый канал — спираль (в native out
// видна горбами 4400+ kbps над потолком CBR). Один IDR в секунду успевает дойти
// и разорвать петлю. Keyframe общий — одного достаточно всем зрителям.
func (h *hub) requestKeyframe() {
	debounce := time.Second
	if v := os.Getenv("KATANA_KEYFRAME_DEBOUNCE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 10000 {
			debounce = time.Duration(n) * time.Millisecond
		}
	}
	h.mu.Lock()
	now := time.Now()
	if !h.lastKeyReq.IsZero() && now.Sub(h.lastKeyReq) < debounce {
		h.keySuppressed++
		h.mu.Unlock()
		return
	}
	h.lastKeyReq = now
	suppressed := h.keySuppressed
	h.keySuppressed = 0
	str := h.str

	// PLI-шторм (повторные PLI при чистых RR) = затор очереди на бутылочном
	// горлышке (bufferbloat): пакеты уже НЕ теряются, но едут с многосекундной
	// задержкой — зритель стоит, а RR-петля видит loss=0% и даже поднимает битрейт.
	// Единственный надёжный сигнал затора здесь — сами повторные PLI: режем битрейт
	// мультипликативно, чтобы очередь стекла быстрее (не чаще раза в 3с).
	stormCut := 0
	if h.autoBitrate && suppressed >= 3 &&
		(h.lastStormCut.IsZero() || now.Sub(h.lastStormCut) >= 3*time.Second) {
		cur := h.curBitrate
		if cur <= 0 {
			cur = h.maxBitrate
		}
		if cut := cur * 4 / 5; cut >= 200 && cut != cur {
			h.lastStormCut = now
			h.curBitrate = cut
			h.cleanTicks = 0
			stormCut = cut
		}
	}
	h.mu.Unlock()
	if str != nil {
		log.Printf("signaling: keyframe forced (PLI/join), подавлено дебаунсом с прошлого: %d", suppressed)
		if stormCut > 0 {
			log.Printf("signaling: PLI-шторм → срез битрейта до %d kbps (разгружаем очередь)", stormCut)
			str.setBitrateKbps(stormCut)
		}
		str.requestKeyframe()
	}
}

// onLoss — адаптивный битрейт (AIMD) по доле потерь из ReceiverReport. Растут
// потери → мультипликативно снижаем (быстро уступаем дорогу), чисто → аддитивно
// поднимаем (осторожно пробуем выше) до потолка настроек. Срабатывает только при
// включённом autoBitrate; дебаунс ~1с (RR приходят примерно раз в секунду).
func (h *hub) onLoss(lost float64) {
	h.mu.Lock()
	// Диагностика: заметные потери логируем ВСЕГДА, даже при выключенном
	// автобитрейте — иначе при фиксированном битрейте RR-потери невидимы в логе
	// и сеть невозможно отличить от проблем захвата/энкода.
	if lost >= 0.05 && time.Since(h.lastLossLog) >= 2*time.Second {
		h.lastLossLog = time.Now()
		log.Printf("signaling: RR loss=%.1f%% (autoBitrate=%v)", lost*100, h.autoBitrate)
	}
	if !h.autoBitrate || h.str == nil {
		h.mu.Unlock()
		return
	}
	now := time.Now()
	if !h.lastBrAdj.IsZero() && now.Sub(h.lastBrAdj) < time.Second {
		h.mu.Unlock()
		return
	}
	h.lastBrAdj = now
	cur := h.curBitrate
	if cur <= 0 {
		cur = h.maxBitrate
	}
	// Decrease быстрый (уступаем дорогу мгновенно), increase медленный и
	// осторожный: поднимаем только после нескольких секунд УСТОЙЧИВОЙ чистоты,
	// малым шагом. Иначе после резкого спада потери на миг = 0, AIMD тут же
	// лезет вверх и снова ловит перегрузку — осцилляция (видно в логах).
	switch {
	case lost > 0.20: // катастрофа — режем вдвое
		cur = cur / 2
		h.cleanTicks = 0
	case lost > 0.05: // умеренные потери — мультипликативный спад
		cur = cur * 4 / 5
		h.cleanTicks = 0
	case lost < 0.02: // чисто — копим уверенность, поднимаем не сразу
		// «Чистые» RR при недавних PLI — ловушка: во время стекания затора пакеты
		// уже не теряются (loss=0%), но зритель ещё стоит и шлёт PLI. Поднимать
		// битрейт в этот момент = продлевать сталл. Пока идут PLI — не растём.
		if !h.lastKeyReq.IsZero() && now.Sub(h.lastKeyReq) < 3*time.Second {
			h.cleanTicks = 0
			break
		}
		h.cleanTicks++
		if h.cleanTicks >= 3 { // ~3с подряд без потерь
			cur += 150
			h.cleanTicks = 0
		}
	default: // 2–5% потерь — держим как есть, не дёргаем
		h.cleanTicks = 0
	}
	if cur < 200 {
		cur = 200
	}
	if h.maxBitrate > 0 && cur > h.maxBitrate {
		cur = h.maxBitrate
	}
	changed := cur != h.curBitrate
	prev := h.curBitrate
	h.curBitrate = cur
	str := h.str
	h.mu.Unlock()
	if changed && str != nil {
		log.Printf("signaling: AIMD loss=%.1f%% bitrate %d→%d kbps", lost*100, prev, cur)
		str.setBitrateKbps(cur)
	}
}

// applyAutoBitrateLocked обновляет режим адаптивного битрейта из настроек зрителя
// и переинициализирует контур: потолок = целевой битрейт настроек, старт с
// потолка. Вызывать под h.mu.
func (h *hub) applyAutoBitrateLocked(msg signalMessage) {
	if msg.Config != nil && msg.Config.AutoBitrate != nil {
		h.autoBitrate = *msg.Config.AutoBitrate
	}
	h.maxBitrate = parseBitrateKbps(h.curOpts.Bitrate)
	if h.curBitrate == 0 || (h.maxBitrate > 0 && h.curBitrate > h.maxBitrate) {
		h.curBitrate = h.maxBitrate
	}
	setPacerBitrateKbps(h.curBitrate) // пейсер стартует от актуального битрейта
}

// parseBitrateKbps вытаскивает kbps из строки опций ("3000k", "3M", "2500").
func parseBitrateKbps(s string) int {
	if s == "" {
		return 0
	}
	mult := 1
	num := s
	switch s[len(s)-1] {
	case 'k', 'K':
		num = s[:len(s)-1]
	case 'm', 'M':
		num = s[:len(s)-1]
		mult = 1000
	}
	var v int
	if _, err := fmt.Sscanf(num, "%d", &v); err != nil {
		return 0
	}
	return v * mult
}

// buildOpts строит опции из hello первого зрителя поверх базовых.
func (h *hub) buildOpts(msg signalMessage) capture.Options {
	return h.applyHello(h.base, msg)
}

// applyHello накладывает config/codec/audio из сообщения на заданные опции.
func (h *hub) applyHello(opts capture.Options, msg signalMessage) capture.Options {
	if msg.Config != nil {
		opts = msg.Config.apply(opts)
	}
	switch msg.Codec {
	case "h264":
		opts.Codec = capture.CodecH264
	case "vp8":
		opts.Codec = capture.CodecVP8
	}
	if msg.Audio != nil {
		opts.Audio = *msg.Audio
	}
	return opts
}

// setSource обновляет кэш геометрии общего источника (для координат мыши).
// Вызывать под h.mu (кроме горутин reconfigure, где берём srcMu отдельно).
func (h *hub) setSource(kind string, id int) {
	r, err := capture.SourceRect(kind, id)
	if err != nil {
		return
	}
	h.srcMu.Lock()
	h.rect = r
	h.srcMu.Unlock()
}

// newSharedTracks создаёт видео-трек (по кодеку) и, если включён звук, Opus-трек.
// Треки общие для всех зрителей — pion раздаёт WriteSample по всем PC, куда они
// добавлены.
func newSharedTracks(opts capture.Options) (*webrtc.TrackLocalStaticSample, *webrtc.TrackLocalStaticSample, error) {
	mime := webrtc.MimeTypeVP8
	if opts.Codec == capture.CodecH264 {
		mime = webrtc.MimeTypeH264
	}
	vtrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: mime}, "screen", "desktop")
	if err != nil {
		return nil, nil, fmt.Errorf("new video track: %w", err)
	}
	var atrack *webrtc.TrackLocalStaticSample
	if opts.Audio {
		// Отдельный streamID (не "desktop") → видео и аудио в РАЗНЫХ MediaStream,
		// Chrome не синхронит A/V и не раздувает видео-буфер под звук.
		atrack, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "audio")
		if err != nil {
			return nil, nil, fmt.Errorf("new audio track: %w", err)
		}
	}
	return vtrack, atrack, nil
}

// webrtcAPI — общий API pion с дефолтными кодеками И интерсепторами. Ключевое:
// RegisterDefaultInterceptors включает NACK-респондер (ретрансляцию потерянных
// пакетов) + RTCP. Без него потерянный пакет keyframe'а никто не досылает —
// декодер зрителя виснет навсегда, хотя звук (Opus, без опорного кадра) идёт.
// Голый webrtc.NewPeerConnection ставит кодеки, но НЕ интерсепторы (в SDP rtx/nack
// есть, а по факту ретрансляции нет — и в GetStats нет OutboundRTPStreamStats).
var webrtcAPI = func() *webrtc.API {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}
	ir := &interceptor.Registry{}
	// Пейсер ВЫКЛЮЧЕН по умолчанию: на практике он вредил — держал пакеты в очереди,
	// зритель читал задержку как потери (RR 50-96% при реальных ~0%) и дропал
	// опоздавшие кадры. Без него зритель декодит чистые 60fps. Включается только
	// принудительно через KATANA_PACER=1 (если вдруг понадобится на шейпленном канале).
	if os.Getenv("KATANA_PACER") == "1" {
		ir.Add(pacerFactory{})
		log.Printf("webrtc: RTP-пейсер включён (KATANA_PACER=1)")
	}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		panic(err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
}()

// buildLocked создаёт PeerConnection зрителя поверх ОБЩИХ треков хаба, вешает
// data-каналы (input/term) и обработчики. Захват уже идёт. Вызывать под h.mu.
func (p *peer) buildLocked() error {
	h := p.h
	// ICE из /api/ice (несколько STUN на разных доменах + TURN, когда появится).
	// Фолбэк на Google STUN зашит в hostICEServers.
	pc, err := webrtcAPI.NewPeerConnection(webrtc.Configuration{
		ICEServers: hostICEServers,
	})
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	p.pc = pc

	vsender, err := pc.AddTrack(h.vtrack)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("add video track: %w", err)
	}
	go readRTCP(vsender, h.requestKeyframe, h.onLoss)
	p.videoSender = vsender

	if h.atrack != nil {
		asender, err := pc.AddTrack(h.atrack)
		if err != nil {
			_ = pc.Close()
			return fmt.Errorf("add audio track: %w", err)
		}
		go readRTCP(asender, nil, nil)
		p.audioSender = asender
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		h.send(signalMessage{Type: "candidate", Candidate: &init}, p.pid)
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("signaling: viewer %s state -> %s", p.pid, state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			// Отвал одного зрителя НЕ рвёт трансляцию остальных — чистим только его.
			go h.removePeer(p.pid)
		}
	})

	// Каналы данных создаём ДО оффера → попадут в SDP.
	if dc, err := pc.CreateDataChannel("input", nil); err != nil {
		log.Printf("signaling: data channel: %v", err)
	} else {
		p.inputDC = dc // для отчёта позиции курсора вьюеру
		dc.OnOpen(func() {
			hn, _ := os.Hostname()
			caps := hostCaps()
			if b, err := json.Marshal(signalMessage{Type: "hostinfo", OS: osLabel(), Hostname: hn, Ffmpeg: capture.FFmpegPath() != "", Video: caps.Video, AudioCap: caps.Audio, Input: caps.Input, Terminal: caps.Terminal, Gamepad: caps.Gamepad, MouseCapture: caps.MouseCapture}); err == nil {
				_ = dc.SendText(string(b))
			}
		})
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if !m.IsString {
				p.dispatchInputBin(m.Data)
				return
			}
			var im signalMessage
			if json.Unmarshal(m.Data, &im) != nil {
				return
			}
			switch im.Type {
			case "sources":
				if src, err := capture.ListSources(); err == nil {
					if b, err := json.Marshal(signalMessage{Type: "sources", Sources: &src}); err == nil {
						_ = dc.SendText(string(b))
					}
				}
			case "activate":
				if im.PID > 0 {
					_ = capture.ActivateApp(im.PID)
				}
			case "ping":
				if b, err := json.Marshal(signalMessage{Type: "pong", T: im.T}); err == nil {
					_ = dc.SendText(string(b))
				}
			case "config":
				p.h.onConfig(im) // зритель меняет общие настройки (источник/разрешение)
			case "renegotiate":
				p.h.onRenegotiate(im) // зритель меняет кодек/аудио
			case "keyframe":
				// Зритель завис (потери на канале) и просит свежий keyframe по
				// data-каналу — надёжнее RTCP PLI, который на потерях сам теряется.
				// Как канал прояснится — запрос дойдёт, декодер догонит без реконнекта.
				p.h.requestKeyframe()
			default:
				p.dispatchInput(&im)
			}
		})
	}
	// input-fast: unordered + unreliable для мыши/осей — без очереди ретрансмитов.
	fastOrdered := false
	fastMaxRetransmits := uint16(0)
	if dc, err := pc.CreateDataChannel("input-fast", &webrtc.DataChannelInit{
		Ordered:        &fastOrdered,
		MaxRetransmits: &fastMaxRetransmits,
	}); err != nil {
		log.Printf("signaling: input-fast channel: %v", err)
	} else {
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if !m.IsString {
				p.dispatchInputFast(m.Data)
			}
		})
	}
	if dc, err := pc.CreateDataChannel("term", nil); err != nil {
		log.Printf("signaling: term channel: %v", err)
	} else {
		sharedTerminal.bind(dc) // терминал общий: PTY один на всех зрителей
	}
	return nil
}

// offer создаёт и отправляет оффер этому зрителю.
func (p *peer) offer() {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("signaling: create offer (%s): %v", p.pid, err)
		return
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		log.Printf("signaling: set local description (%s): %v", p.pid, err)
		return
	}
	p.h.send(signalMessage{Type: "offer", SDP: offer.SDP}, p.pid)
}

// closePC закрывает PeerConnection зрителя.
func (p *peer) closePC() {
	if p.pc != nil {
		if err := p.pc.Close(); err != nil {
			log.Printf("signaling: pc close (%s): %v", p.pid, err)
		}
		p.pc = nil
	}
}

// inputDebug — KATANA_INPUT_DEBUG=1 включает лог сырых mouse-событий.
var inputDebug = os.Getenv("KATANA_INPUT_DEBUG") != ""

// dispatchInput обрабатывает события ввода (mouse/scroll/cursor/key/type) — общий
// путь для DataChannel (основной) и WebSocket (фолбэк).
func (p *peer) dispatchInput(msg *signalMessage) {
	switch msg.Type {
	case "mouse":
		if msg.Mouse != nil {
			// KATANA_INPUT_DEBUG=1: сырой поток mouse-событий (отладка мобильного
			// тача — «курсор улетает в угол»).
			if inputDebug {
				log.Printf("input: mouse action=%q x=%.4f y=%.4f dx=%d dy=%d btn=%q",
					msg.Mouse.Action, msg.Mouse.X, msg.Mouse.Y, msg.Mouse.Dx, msg.Mouse.Dy, msg.Mouse.Button)
			}
			p.handleMouse(msg.Mouse)
		}
	case "vcursor":
		// Позиция СОБСТВЕННОГО указателя зрителя над видео — только запоминаем для
		// рассылки остальным (broadcastCursors). OS-курсор хоста НЕ трогаем.
		p.h.mu.Lock()
		if msg.Gone || msg.Mouse == nil {
			p.vcActive = false
		} else {
			p.vcX, p.vcY = clampF(msg.Mouse.X), clampF(msg.Mouse.Y)
			p.vcTS = time.Now()
			p.vcActive = true
		}
		p.h.mu.Unlock()
	case "vcursorname":
		// Ник зрителя для подписи его курсора у остальных (шлётся один раз). Режем
		// длину — это чужой ввод, попадёт в UI других зрителей.
		name := msg.Name
		if len(name) > 32 {
			name = name[:32]
		}
		p.h.mu.Lock()
		p.vcName = name
		p.h.mu.Unlock()
	case "scroll":
		if msg.Scroll != nil {
			scrollMouse(msg.Scroll.Dx, msg.Scroll.Dy)
		}
	case "zoom":
		// Пинч на мобиле → Cmd +/− (зум страницы в браузере и др. приложениях;
		// настоящий magnify-жест macOS инжектить нельзя).
		if msg.Dir == "out" {
			tapKey("-", []string{"cmd"})
		} else {
			tapKey("=", []string{"cmd"})
		}
	case "cursor":
		// Курсор хоста общий для захвата — меняем на лету у всех.
		p.h.mu.Lock()
		str := p.h.str
		p.h.mu.Unlock()
		if str != nil && msg.Config != nil && msg.Config.Cursor != nil {
			str.updateCursor(*msg.Config.Cursor)
		}
	case "key":
		if msg.Key != nil && msg.Key.Key != "" {
			tapKey(msg.Key.Key, msg.Key.Mods)
		}
	case "keydown":
		if msg.Key != nil && msg.Key.Key != "" {
			keyDown(msg.Key.Key, msg.Key.Mods)
		}
	case "keyup":
		if msg.Key != nil && msg.Key.Key != "" {
			keyUp(msg.Key.Key, msg.Key.Mods)
		}
	case "type":
		if msg.Text != "" {
			typeText(msg.Text)
		}
	case "gamepad":
		if msg.Pad != nil {
			switch msg.Pad.Kind {
			case "btn":
				gamepadButton(msg.Pad.Idx, msg.Pad.Down, msg.Pad.Val)
			case "axis":
				gamepadAxis(msg.Pad.Idx, msg.Pad.Val)
			}
		}
	case "capture":
		// Вьювер захватил/отпустил указатель (Pointer Lock в игре). Роутинг движения
		// stateless по action ("rel" vs "move") — этот флаг лишь для housekeeping:
		// не слать назад cursorpos в захвате и отпустить кнопки при выходе.
		p.captureActive = msg.On
		if !msg.On {
			releaseAllButtons()
		}
	}
}

// dispatchInputFast декодирует бинарные сообщения с канала input-fast
// (unordered+unreliable): движение мыши, оси геймпада, триггеры.
//
//	0x01  mouse abs:     [x float32le] [y float32le]          → 9 байт
//	0x02  mouse rel:     [dx int16le] [dy int16le] [drag u8]  → 6 байт
//	0x04  gp axis:       [idx u8] [val int16le]               → 4 байта
//	0x05  gp trigger:    [idx u8] [down u8] [val u8]          → 4 байта
func (p *peer) dispatchInputFast(data []byte) {
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case 0x01: // mouse abs (hover/move)
		if len(data) < 9 {
			return
		}
		x := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[1:5])))
		y := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[5:9])))
		p.handleMouse(&mouseMsg{X: x, Y: y, Action: "move"})
	case 0x02: // mouse rel (moverel / dragrel — трекпад мобилы)
		if len(data) < 6 {
			return
		}
		dx := int(int16(binary.LittleEndian.Uint16(data[1:3])))
		dy := int(int16(binary.LittleEndian.Uint16(data[3:5])))
		action := "moverel"
		if data[5] != 0 {
			action = "dragrel"
		}
		p.handleMouse(&mouseMsg{Action: action, Dx: dx, Dy: dy})
	case 0x04: // gamepad axis (стики)
		if len(data) < 4 {
			return
		}
		idx := int(data[1])
		val := float64(int16(binary.LittleEndian.Uint16(data[2:4]))) / 32767.0
		gamepadAxis(idx, val)
	case 0x05: // gamepad trigger (btn 6/7 — аналоговые)
		if len(data) < 4 {
			return
		}
		idx := int(data[1])
		down := data[2] != 0
		val := float64(data[3]) / 255.0
		gamepadButton(idx, down, val)
	}
}

// handleMouse мапит нормализованные координаты в глобальные и инжектит событие.
// Геометрия источника общая (один захват), drag-состояние — своё на зрителя.
// reportCursor сообщает вьюеру текущую позицию курсора (нормализованную к
// прямоугольнику источника) — для подсветки курсора и follow-pan при зуме на
// мобиле. Троттлинг ~30/с, чтобы не флудить канал.
func (p *peer) reportCursor() {
	if p.inputDC == nil || p.captureActive {
		return // в захвате абсолютной позиции курсора нет — кольцо не шлём
	}
	now := time.Now()
	if now.Sub(p.lastCursorReport) >= 25*time.Millisecond {
		p.lastCursorReport = now
		p.sendCursor()
	}
	// Трейлинг: когда движение стихнет, дослать ТОЧНУЮ финальную позицию (иначе
	// последний кадр движения мог попасть в троттл-окно → кольцо отстаёт).
	if p.cursorTimer != nil {
		p.cursorTimer.Stop()
	}
	p.cursorTimer = time.AfterFunc(70*time.Millisecond, p.sendCursor)
}

// sendCursor шлёт текущую позицию курсора (норм. к прямоугольнику источника).
func (p *peer) sendCursor() {
	if p.inputDC == nil {
		return
	}
	p.h.srcMu.Lock()
	r := p.h.rect
	p.h.srcMu.Unlock()
	if r.W <= 0 || r.H <= 0 {
		return
	}
	x, y := mouseLocation()
	cx := clampF((float64(x) - r.X) / r.W)
	cy := clampF((float64(y) - r.Y) / r.H)
	if b, err := json.Marshal(signalMessage{Type: "cursorpos", Mouse: &mouseMsg{X: cx, Y: cy}}); err == nil {
		_ = p.inputDC.SendText(string(b))
	}
}

func (p *peer) handleMouse(m *mouseMsg) {
	// Трекпад-режим (мобила): движение/клик/перетаскивание относительно ТЕКУЩЕЙ
	// позиции, без маппинга на прямоугольник источника.
	//  moverel — свайп (свободное движение); click — тап; press/release —
	//  зажать/отпустить кнопку (long-press-drag); dragrel — свайп с зажатой.
	btn := "left"
	if m.Button == "right" {
		btn = "right"
	}
	switch m.Action {
	case "rel":
		// Захват мыши для игр: сырые дельты → relative-устройство. Без reportCursor
		// (в захвате нет осмысленной абсолютной позиции курсора).
		moveRelRaw(m.Dx, m.Dy)
		return
	case "moverel":
		moveRel(m.Dx, m.Dy)
		p.reportCursor()
		return
	case "click":
		clickMouse(btn)
		return
	case "dblclick":
		doubleClick(btn)
		return
	case "press":
		mouseToggle(btn, true)
		return
	case "dragrel":
		dragRel(m.Dx, m.Dy, btn)
		p.reportCursor()
		return
	case "release":
		mouseToggle(btn, false)
		return
	}

	p.h.srcMu.Lock()
	r := p.h.rect
	p.h.srcMu.Unlock()
	if r.W <= 0 || r.H <= 0 {
		return
	}
	x := int(r.X + clampF(m.X)*r.W)
	y := int(r.Y + clampF(m.Y)*r.H)
	button := "left"
	if m.Button == "right" {
		button = "right"
	}
	switch m.Action {
	case "down":
		moveMouse(x, y)
		mouseToggle(button, true)
		p.btnDown = button
		p.dragged = false
	case "up":
		// dragMouse шлём ТОЛЬКО если реально был drag (приходили move). Иначе
		// (чистый клик) — просто отпускаем: без события Dragged, чтобы тап не
		// принимался за перетаскивание/выделение.
		if p.btnDown != "" && p.dragged {
			dragMouse(x, y, p.btnDown)
		}
		mouseToggle(button, false)
		p.btnDown = ""
		p.dragged = false
	default: // move
		if p.btnDown != "" {
			dragMouse(x, y, p.btnDown) // зажата кнопка → drag-событие
			p.dragged = true
		} else {
			moveMouse(x, y)
		}
	}
}

// dispatchInputBin декодирует бинарные сообщения с канала input.
//
//	0x06  kb state: [count u8] [hid_code u8 × count]
func (p *peer) dispatchInputBin(data []byte) {
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case 0x06: // keyboard state
		if len(data) < 2 {
			return
		}
		n := int(data[1])
		if len(data) < 2+n {
			return
		}
		p.applyKbState(data[2 : 2+n])
	}
}

// applyKbState дифает новый набор HID-кодов с предыдущим и инжектит press/release.
func (p *peer) applyKbState(codes []byte) {
	next := make(map[uint8]bool, len(codes))
	for _, c := range codes {
		next[c] = true
	}
	if p.kbState == nil {
		p.kbState = map[uint8]bool{}
	}
	var ups, downs []uint8
	for c := range p.kbState {
		if !next[c] {
			keyUpHID(c)
			ups = append(ups, c)
		}
	}
	for c := range next {
		if !p.kbState[c] {
			keyDownHID(c)
			downs = append(downs, c)
		}
	}
	p.kbState = next
	// Отладка состояния клавиатуры (--kbd-debug): полный набор зажатых + лента
	// событий со стрелками — в TUI (uiKbd) и в лог-файл. Помогает ловить «залипшие»
	// модификаторы (keyup потерян браузером на системном шорткате) и расхождения
	// кода/символа.
	if kbdDebug {
		sort.Slice(downs, func(i, j int) bool { return downs[i] < downs[j] })
		sort.Slice(ups, func(i, j int) bool { return ups[i] < ups[j] })
		events := make([]string, 0, len(downs)+len(ups))
		for _, c := range downs {
			events = append(events, "↓"+hidName(c))
		}
		for _, c := range ups {
			events = append(events, "↑"+hidName(c))
		}
		held := fmtHIDSet(next)
		log.Printf("kbd: held=%s %s", held, strings.Join(events, " "))
		uiKbd(held, events)
	}
}

// kbdDebug включает подробный лог состояния клавиатуры (флаг --kbd-debug).
var kbdDebug bool

// hidNames — HID Usage ID → человекочитаемое имя/символ (для отладочного лога;
// платформо-нейтрально, отдельно от инъекции ввода в input_*.go).
var hidNames = map[uint8]string{
	0x04: "a", 0x05: "b", 0x06: "c", 0x07: "d", 0x08: "e", 0x09: "f",
	0x0A: "g", 0x0B: "h", 0x0C: "i", 0x0D: "j", 0x0E: "k", 0x0F: "l",
	0x10: "m", 0x11: "n", 0x12: "o", 0x13: "p", 0x14: "q", 0x15: "r",
	0x16: "s", 0x17: "t", 0x18: "u", 0x19: "v", 0x1A: "w", 0x1B: "x",
	0x1C: "y", 0x1D: "z",
	0x1E: "1", 0x1F: "2", 0x20: "3", 0x21: "4", 0x22: "5",
	0x23: "6", 0x24: "7", 0x25: "8", 0x26: "9", 0x27: "0",
	0x28: "enter", 0x29: "esc", 0x2A: "bksp", 0x2B: "tab", 0x2C: "space",
	0x2D: "-", 0x2E: "=", 0x2F: "[", 0x30: "]", 0x31: "\\",
	0x33: ";", 0x34: "'", 0x35: "`", 0x36: ",", 0x37: ".", 0x38: "/",
	0x39: "caps",
	0x3A: "f1", 0x3B: "f2", 0x3C: "f3", 0x3D: "f4", 0x3E: "f5", 0x3F: "f6",
	0x40: "f7", 0x41: "f8", 0x42: "f9", 0x43: "f10", 0x44: "f11", 0x45: "f12",
	0x49: "ins", 0x4A: "home", 0x4B: "pgup",
	0x4C: "del", 0x4D: "end", 0x4E: "pgdn",
	0x4F: "→", 0x50: "←", 0x51: "↓", 0x52: "↑",
	0xE0: "ctrl", 0xE1: "shift", 0xE2: "alt", 0xE3: "cmd",
	0xE4: "rctrl", 0xE5: "rshift", 0xE6: "ralt", 0xE7: "rcmd",
}

func hidName(c uint8) string {
	if n, ok := hidNames[c]; ok {
		return n
	}
	return "?"
}

// fmtHIDSet форматирует набор зажатых кодов как "[0x09:f 0xE3:cmd]" (по возрастанию).
func fmtHIDSet(set map[uint8]bool) string {
	codes := make([]int, 0, len(set))
	for c := range set {
		codes = append(codes, int(c))
	}
	sort.Ints(codes)
	var b strings.Builder
	b.WriteByte('[')
	for i, c := range codes {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "0x%02X:%s", c, hidName(uint8(c)))
	}
	b.WriteByte(']')
	return b.String()
}

// fmtHIDList форматирует дельту (нажатые/отпущенные) с префиксом; "" если пусто.
func fmtHIDList(prefix string, codes []uint8) string {
	if len(codes) == 0 {
		return ""
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	var b strings.Builder
	for i, c := range codes {
		if i == 0 {
			b.WriteString(prefix)
		} else {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "0x%02X:%s", c, hidName(c))
	}
	return b.String()
}

// releaseKbState отпускает все зажатые клавиши зрителя (при дисконнекте).
func (p *peer) releaseKbState() {
	for c := range p.kbState {
		keyUpHID(c)
	}
	p.kbState = nil
}
