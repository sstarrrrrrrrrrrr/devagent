package tui

import (
	"context"
	"devagent/agent"
	"devagent/session"
	"regexp"
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

// 用于 Markdown 渲染的样式与正则。
var (
	mdHeading    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	mdCodeBlock  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	mdInlineCode = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Background(lipgloss.Color("236"))
	mdBold       = lipgloss.NewStyle().Bold(true)
	mdItalic     = lipgloss.NewStyle().Italic(true)
	mdQuote      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	mdLink       = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Underline(true)

	headingRe = regexp.MustCompile(`^#{1,6}\s`)
	hrRe      = regexp.MustCompile(`^\s*([-*_]\s*){3,}$`)
	inlineRe  = regexp.MustCompile("`[^`]+`|\\*\\*[^*]+\\*\\*|\\*[^*]+\\*|\\[[^\\]]+\\]\\([^)]*\\)")
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
			b.WriteString(msg.role + ":")
		}
		b.WriteString("\n")
		if msg.role == "agent" {
			b.WriteString(renderMarkdown(msg.content))
		} else {
			b.WriteString(msg.content)
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// renderMarkdown 把模型的 Markdown 回答渲染成带 ANSI 样式的文本。
// 只处理 LLM 输出里最常见的元素：代码块、标题、分隔线、引用、加粗、斜体、行内代码、链接。
func renderMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var b strings.Builder
	inCode := false
	for _, line := range lines {
		t := strings.TrimRight(line, " \t")
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString(mdCodeBlock.Render(t))
			b.WriteString("\n")
			continue
		}
		switch {
		case headingRe.MatchString(t):
			b.WriteString(mdHeading.Render(t))
		case hrRe.MatchString(t):
			b.WriteString(strings.Repeat("─", 40))
		case strings.HasPrefix(t, ">"):
			b.WriteString(mdQuote.Render(strings.TrimSpace(strings.TrimPrefix(t, ">"))))
		case strings.TrimSpace(t) == "":
			// 空行，仅换行。
		default:
			b.WriteString(renderInline(t))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderInline 处理行内的加粗、斜体、行内代码和链接。
func renderInline(s string) string {
	return inlineRe.ReplaceAllStringFunc(s, func(m string) string {
		switch {
		case strings.HasPrefix(m, "`") && strings.HasSuffix(m, "`") && len(m) >= 2:
			return mdInlineCode.Render(m[1 : len(m)-1])
		case strings.HasPrefix(m, "**") && strings.HasSuffix(m, "**") && len(m) >= 4:
			return mdBold.Render(m[2 : len(m)-2])
		case strings.HasPrefix(m, "[") && strings.HasSuffix(m, ")"):
			inner := m[1 : len(m)-1]
			if i := strings.Index(inner, "]("); i >= 0 {
				return mdLink.Render(inner[:i])
			}
			return m
		case strings.HasPrefix(m, "*") && strings.HasSuffix(m, "*") && len(m) >= 2:
			return mdItalic.Render(m[1 : len(m)-1])
		}
		return m
	})
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
	defer model.agent.Close()
	P := tea.NewProgram(model)
	_, err = P.Run()
	return err
}
