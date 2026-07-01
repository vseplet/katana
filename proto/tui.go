package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	logPath string
	status  string
	owner   string
	plan    string
	viewers int
	list    []viewerCount
	sp      spinner.Model
	cancel  context.CancelFunc
}

func newHostModel(session, logPath string, cancel context.CancelFunc) hostModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	return hostModel{session: session, logPath: logPath, status: "starting…", sp: sp, cancel: cancel}
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
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render("katana")
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	val := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	sess := m.session
	if len(sess) > 8 {
		sess = sess[:8]
	}

	var b strings.Builder
	b.WriteString(title + "  " + dim.Render("host") + "\n\n")
	b.WriteString(m.sp.View() + val.Render(m.status) + "\n\n")
	b.WriteString(label.Render("session  ") + val.Render(sess) + "\n")
	if m.owner != "" {
		b.WriteString(label.Render("owner    ") + val.Render(m.owner) + " " + planBadge(m.plan) + "\n")
	}
	b.WriteString(label.Render("viewers  ") + val.Render(fmt.Sprintf("%d", m.viewers)) + "\n")
	for _, v := range m.list {
		b.WriteString(label.Render("         ") + val.Render(v.Name) + dim.Render(fmt.Sprintf("  ×%d", v.Views)) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(dim.Render("logs: "+m.logPath) + "\n")
	b.WriteString(dim.Render("press q to quit"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("238")).
		Padding(1, 3).
		Render(b.String()) + "\n"
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
func runHostUI(session, logPath string, cancel context.CancelFunc, run func()) {
	uiProg = tea.NewProgram(newHostModel(session, logPath, cancel))
	go func() {
		run()
		uiProg.Quit()
	}()
	_, _ = uiProg.Run()
	uiProg = nil
}
