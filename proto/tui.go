package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mdp/qrterminal/v3"
)

// TUI хоста: вместо потока логов — компактный живой статус (bubbletea). Логи
// уходят в файл (~/.katana/<session>.log). uiProg != nil только когда TUI запущен
// (есть TTY); тогда статус-хелперы шлют туда обновления из горутин хоста.
var uiProg *tea.Program

type statusMsg string
type viewersMsg int

// uiStatus/uiViewers — потокобезопасные апдейты TUI из любых горутин (tea.Send
// сам синхронизирует). No-op, если TUI не запущен (fallback без TTY).
func uiStatus(s string) {
	if uiProg != nil {
		uiProg.Send(statusMsg(s))
	}
}

func uiViewers(n int) {
	if uiProg != nil {
		uiProg.Send(viewersMsg(n))
	}
}

type ownerMsg struct {
	owner, plan string
}
type viewerListMsg []viewerCount

// uiOwner — владелец сессии + уровень подписки (из брокера). uiViewerList —
// список зрителей с числом вкладок (из брокера, presence).
func uiOwner(owner, plan string) {
	if uiProg != nil {
		uiProg.Send(ownerMsg{owner: owner, plan: plan})
	}
}

func uiViewerList(v []viewerCount) {
	if uiProg != nil {
		uiProg.Send(viewerListMsg(v))
	}
}

type hostModel struct {
	session string
	url     string // ссылка зрителя (в QR)
	qr      string // предрендеренный QR-код (half-blocks)
	logPath string
	status  string
	owner   string
	plan    string
	viewers int
	list    []viewerCount
	showQR  bool // QR большой — по умолчанию скрыт, по кнопке c (выводится снизу)
	sp      spinner.Model
	cancel  context.CancelFunc
}

func newHostModel(session, url, logPath string, cancel context.CancelFunc) hostModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	return hostModel{session: session, url: url, qr: makeQR(url), logPath: logPath, status: "starting…", sp: sp, cancel: cancel}
}

// makeQR рендерит компактный QR (half-blocks) в строку для вывода в TUI.
func makeQR(url string) string {
	var buf bytes.Buffer
	qrterminal.GenerateWithConfig(url, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         &buf,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		QuietZone:      1,
	})
	return strings.Trim(buf.String(), "\n")
}

func (m hostModel) Init() tea.Cmd { return m.sp.Tick }

func (m hostModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			if m.cancel != nil {
				m.cancel() // останавливаем хост
			}
			return m, tea.Quit
		case "c":
			m.showQR = !m.showQR
			return m, nil
		}
	case statusMsg:
		m.status = string(msg)
	case viewersMsg:
		m.viewers = int(msg)
	case ownerMsg:
		m.owner = msg.owner
		m.plan = msg.plan
	case viewerListMsg:
		m.list = []viewerCount(msg)
		m.viewers = len(m.list)
	default:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m hostModel) View() string {
	pink := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	lbl := lipgloss.NewStyle().Foreground(lipgloss.Color("103")) // grey-blue лейблы
	val := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	cyan := lipgloss.NewStyle().Foreground(lipgloss.Color("80"))
	name := lipgloss.NewStyle().Foreground(lipgloss.Color("114")) // зелёные имена
	sep := dim.Render(" · ")
	stat := lipgloss.NewStyle().Bold(true).Foreground(statusColor(m.status))

	sess := m.session
	if len(sess) > 8 {
		sess = sess[:8]
	}

	var b strings.Builder
	b.WriteString(pink.Render("katana") + "  " + dim.Render("host") + "\n\n")
	b.WriteString(m.sp.View() + stat.Render(m.status) + "\n\n")
	b.WriteString(lbl.Render("session  ") + cyan.Render(sess) + "\n")
	if m.owner != "" {
		b.WriteString(lbl.Render("owner    ") + val.Render(m.owner) + " " + planBadge(m.plan) + "\n")
	}
	// Все зрители одной строкой: users  alice ×2 · bob · guest
	b.WriteString(lbl.Render("users    "))
	if len(m.list) == 0 {
		b.WriteString(dim.Render("none yet"))
	} else {
		parts := make([]string, 0, len(m.list))
		for _, v := range m.list {
			s := name.Render(v.Name)
			if v.Views > 1 {
				s += dim.Render(fmt.Sprintf(" ×%d", v.Views))
			}
			parts = append(parts, s)
		}
		b.WriteString(strings.Join(parts, sep))
	}
	b.WriteString("\n\n")
	// QR со ссылкой зрителя — большой, поэтому только по кнопке c, и выводится снизу.
	if m.showQR {
		b.WriteString(m.qr + "\n" + cyan.Render("scan to watch") + "\n\n")
	}
	b.WriteString(dim.Render("logs: "+m.logPath) + "\n")
	if m.showQR {
		b.WriteString(dim.Render("press c to hide QR · q to quit"))
	} else {
		b.WriteString(dim.Render("press c for connect QR · q to quit"))
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("61")).
		Padding(1, 3).
		Render(b.String()) + "\n"
}

// statusColor — цвет статуса по смыслу: живой=зелёный, ожидание=янтарь,
// остановлен/ошибка=красный.
func statusColor(s string) lipgloss.Color {
	switch {
	case strings.Contains(s, "live"):
		return lipgloss.Color("42") // green
	case strings.Contains(s, "stopped"), strings.Contains(s, "cannot"), strings.Contains(s, "lost"):
		return lipgloss.Color("203") // red
	case strings.Contains(s, "connect"), strings.Contains(s, "waiting"), strings.Contains(s, "reconnect"), strings.Contains(s, "starting"):
		return lipgloss.Color("214") // amber
	default:
		return lipgloss.Color("252")
	}
}

// planBadge — цветной бейдж уровня подписки.
func planBadge(plan string) string {
	if plan == "pro" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("236")).Background(lipgloss.Color("178")).Bold(true).Padding(0, 1).Render("PRO")
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Background(lipgloss.Color("240")).Padding(0, 1).Render("FREE")
}

// runHostUI запускает TUI и хост параллельно: хост крутится в горутине, TUI держит
// главный поток (tea.Run блокирует). Когда хост завершается — закрываем TUI.
func runHostUI(session, url, logPath string, cancel context.CancelFunc, run func()) {
	uiProg = tea.NewProgram(newHostModel(session, url, logPath, cancel))
	go func() {
		run()
		uiProg.Quit()
	}()
	_, _ = uiProg.Run()
	uiProg = nil
}
