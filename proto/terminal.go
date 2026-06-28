package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/pion/webrtc/v4"
)

// Общий терминал поверх WebRTC DataChannel "term": ОДИН login-шелл хоста на всех
// зрителей (как общий tmux). Вывод PTY бродкастится во все подключённые
// term-каналы; ввод от любого зрителя идёт в этот же шелл. Поднимается лениво
// (на первый resize) и живёт, пока работает сервер или пока шелл не завершится.
//
// Протокол канала:
//   - бинарный месседж: сырые байты (server→client = вывод PTY, client→server = stdin);
//   - текстовый месседж (JSON): {t:"resize",cols,rows} | {t:"kill"}.
//
// Размер PTY = минимум по всем подключённым зрителям (как в tmux), чтобы контент
// помещался у всех. Новый зритель получает реплей текущего экрана из кольцевого
// буфера. ⚠️ Любой зритель управляет общим шеллом — это RCE, см. вопрос auth.

const termRingCap = 128 * 1024 // последний экран для реплея при подключении

type termCtl struct {
	T    string `json:"t"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type termClient struct {
	cols, rows uint16
}

type sharedTerm struct {
	mu      sync.Mutex
	ptmx    *os.File
	cmd     *exec.Cmd
	ring    []byte
	clients map[*webrtc.DataChannel]*termClient
}

var sharedTerminal = &sharedTerm{clients: map[*webrtc.DataChannel]*termClient{}}

// bind вешает обработчики на term-канал нового подключения.
func (t *sharedTerm) bind(dc *webrtc.DataChannel) {
	dc.OnMessage(func(msg webrtc.DataChannelMessage) { t.onMessage(dc, msg) })
	dc.OnClose(func() { t.detach(dc) })
}

func (t *sharedTerm) onMessage(dc *webrtc.DataChannel, msg webrtc.DataChannelMessage) {
	if msg.IsString {
		var c termCtl
		if json.Unmarshal(msg.Data, &c) != nil {
			return
		}
		switch c.T {
		case "resize":
			t.attachOrResize(dc, c.Cols, c.Rows)
		case "kill":
			t.kill()
		}
		return
	}
	// Бинарный месседж = stdin в общий шелл.
	t.mu.Lock()
	p := t.ptmx
	t.mu.Unlock()
	if p != nil {
		_, _ = p.Write(msg.Data)
	}
}

// attachOrResize регистрирует зрителя (первый resize = подключение, с реплеем
// текущего экрана) либо обновляет его размер. Всё под mu, чтобы реплей не
// перемешался с бродкастом для нового зрителя.
func (t *sharedTerm) attachOrResize(dc *webrtc.DataChannel, cols, rows uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cl := t.clients[dc]
	newClient := cl == nil
	if newClient {
		cl = &termClient{}
	}
	cl.cols, cl.rows = cols, rows
	t.ensureStartedLocked(cols, rows)

	if newClient {
		// Реплей текущего экрана ДО регистрации в бродкаст (clear + ring),
		// иначе новый зритель может получить свежий чанк раньше истории.
		_ = sendChunked(dc, []byte("\x1b[2J\x1b[H"))
		_ = sendChunked(dc, t.ring)
		t.clients[dc] = cl
	}
	t.applySizeLocked()
}

// ensureStartedLocked поднимает общий PTY один раз (под mu).
func (t *sharedTerm) ensureStartedLocked(cols, rows uint16) {
	if t.ptmx != nil {
		return
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	c := exec.Command(shell, "-l") // login-шелл: подхватит профиль пользователя
	c.Env = append(os.Environ(), "TERM=xterm-256color")
	if home, err := os.UserHomeDir(); err == nil {
		c.Dir = home
	}
	size := &pty.Winsize{Cols: cols, Rows: rows}
	if cols == 0 || rows == 0 {
		size = &pty.Winsize{Cols: 80, Rows: 24}
	}
	ptmx, err := pty.StartWithSize(c, size)
	if err != nil {
		log.Printf("term: start pty: %v", err)
		return
	}
	t.ptmx = ptmx
	t.cmd = c
	log.Printf("term: shared pty started (%s)", shell)
	go t.pump(ptmx)
}

// applySizeLocked выставляет размер PTY = минимум по всем зрителям (под mu).
func (t *sharedTerm) applySizeLocked() {
	if t.ptmx == nil || len(t.clients) == 0 {
		return
	}
	var mc, mr uint16
	for _, c := range t.clients {
		if c.cols == 0 || c.rows == 0 {
			continue
		}
		if mc == 0 || c.cols < mc {
			mc = c.cols
		}
		if mr == 0 || c.rows < mr {
			mr = c.rows
		}
	}
	if mc > 0 && mr > 0 {
		_ = pty.Setsize(t.ptmx, &pty.Winsize{Cols: mc, Rows: mr})
	}
}

// pump читает вывод PTY, копит в кольцевой буфер и бродкастит всем зрителям.
func (t *sharedTerm) pump(ptmx *os.File) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			t.broadcast(buf[:n])
		}
		if err != nil {
			break // шелл завершился (например, exit)
		}
	}
	t.reset()
}

func (t *sharedTerm) broadcast(b []byte) {
	t.mu.Lock()
	t.ring = append(t.ring, b...)
	if len(t.ring) > termRingCap {
		t.ring = t.ring[len(t.ring)-termRingCap:]
	}
	clients := make([]*webrtc.DataChannel, 0, len(t.clients))
	for dc := range t.clients {
		clients = append(clients, dc)
	}
	t.mu.Unlock()
	for _, dc := range clients {
		_ = sendChunked(dc, b) // отвалившийся клиент отцепится по OnClose
	}
}

func (t *sharedTerm) detach(dc *webrtc.DataChannel) {
	t.mu.Lock()
	delete(t.clients, dc)
	t.applySizeLocked()
	t.mu.Unlock()
}

// kill завершает общий шелл по запросу зрителя.
func (t *sharedTerm) kill() {
	t.mu.Lock()
	cmd, ptmx := t.cmd, t.ptmx
	t.cmd, t.ptmx, t.ring = nil, nil, nil
	t.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if ptmx != nil {
		_ = ptmx.Close()
	}
}

// reset сбрасывает состояние после смерти шелла; следующий resize поднимет новый.
func (t *sharedTerm) reset() {
	t.mu.Lock()
	t.cmd, t.ptmx, t.ring = nil, nil, nil
	t.mu.Unlock()
}

// sendChunked шлёт данные кусками (лимит размера SCTP-месседжа DataChannel).
func sendChunked(dc *webrtc.DataChannel, data []byte) error {
	const max = 16 * 1024
	for len(data) > 0 {
		n := len(data)
		if n > max {
			n = max
		}
		if err := dc.Send(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
