package tui

import (
	"context"
	"devagent/agent"
	"devagent/session"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	userStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			Bold(true)

	agentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("42")).
			Bold(true)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)
)

type chatMessage struct {
	role    string
	content string
}

type Model struct {
	input    textinput.Model
	viewport viewport.Model
	spinner  spinner.Model
	agent    *agent.Agent
	session  *session.Session
	messages []chatMessage
	busy     bool
	width    int
	height   int
	ready    bool
}

// agentResultMsg 是 Agent 完成后返回给 TUI 的消息。
type agentResultMsg struct {
	answer string
	err    error
}

const agentTimeout = 3 * time.Minute

func NewModel() (Model, error) {
	input := textinput.New()
	input.CharLimit = 2000
	input.Focus()

	a, err := agent.NewAgent()
	if err != nil {
		return Model{}, err
	}

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return Model{
		input:    input,
		viewport: viewport.New(),
		spinner:  sp,
		agent:    a,
		session:  session.NewSession(),
		messages: []chatMessage{},
	}, nil
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

func askAgentCmd(a *agent.Agent, s *session.Session, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), agentTimeout)
		defer cancel()
		answer, err := a.RunAgent(ctx, s, prompt)
		return agentResultMsg{answer: answer, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewport()
		m.refreshViewport()
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			if m.busy {
				return m, nil
			}
			prompt := strings.TrimSpace(m.input.Value())
			if prompt == "" {
				return m, nil
			}
			m.messages = append(m.messages, chatMessage{role: "user", content: prompt})
			m.input.Reset()
			m.busy = true
			m.refreshViewport()
			return m, tea.Batch(
				askAgentCmd(m.agent, m.session, prompt),
				m.spinner.Tick,
			)
		}

		if isScrollKey(msg) {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case agentResultMsg:
		m.busy = false
		switch {
		case msg.err != nil:
			m.messages = append(m.messages, chatMessage{role: "error", content: msg.err.Error()})
		case strings.TrimSpace(msg.answer) != "":
			m.messages = append(m.messages, chatMessage{role: "agent", content: msg.answer})
		default:
			m.messages = append(m.messages, chatMessage{role: "agent", content: "模型未返回"})
		}
		m.refreshViewport()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) resizeViewport() {
	h := m.height - 2
	if h < 1 {
		h = 1
	}
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(h)
	m.viewport.SoftWrap = true
	m.ready = true
}

func (m *Model) refreshViewport() {
	m.viewport.SetContent(renderMessages(m.messages))
	m.viewport.GotoBottom()
}

func renderMessages(messages []chatMessage) string {
	var b strings.Builder
	b.WriteString("DevAgent —— AI 编程助手\n\n")
	for _, msg := range messages {
		switch msg.role {
		case "user":
			b.WriteString(userStyle.Render("你："))
		case "agent":
			b.WriteString(agentStyle.Render("小哈："))
		case "error":
			b.WriteString(errorStyle.Render("错误："))
		default:
			b.WriteString(msg.role + ": ")
		}
		b.WriteString(msg.content)
		b.WriteString("\n")
	}
	return b.String()
}

func isScrollKey(msg tea.KeyPressMsg) bool {
	switch msg.Key().Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
		return true
	}
	return false
}

func (m Model) View() tea.View {
	content := "加载中…\n"
	if m.ready {
		status := "Enter 发送 · Ctrl+C 退出"
		if m.busy {
			status = m.spinner.View() + " 思考中…（模型可能正在调用工具）"
		}
		content = m.viewport.View() + "\n" + status + "\n" + m.input.View()
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func Run() error {
	model, err := NewModel()
	if err != nil {
		return err
	}
	P := tea.NewProgram(model)
	_, err = P.Run()
	return err
}
