package tui

import (
	"context"
	"devagent/agent"
	"devagent/session"
	"fmt"
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
	mdTableBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	headingRe    = regexp.MustCompile(`^#{1,6}\s`)
	hrRe         = regexp.MustCompile(`^\s*([-*_]\s*){3,}$`)
	tableRowRe   = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	tableSepRe   = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)+\|?\s*$`)
	inlineCodeRe = regexp.MustCompile("`[^`]+`")
	inlineBoldRe = regexp.MustCompile(`\*\*[^*]+\*\*`)
	inlineItalicRe = regexp.MustCompile(`\*[^*]+\*`)
	inlineLinkRe = regexp.MustCompile(`\[[^\]]+\]\([^)]*\)`)
)

// mdBlock 表示 Markdown 中一个独立的块。
type mdBlock struct {
	kind  string   // "code" | "table" | "normal"
	lines []string // 块内原始行
	lang  string   // 仅 code 块：语言标记
}

// tableAlign 表示表格某列的对齐方式。
type tableAlign int

const (
	alignLeft tableAlign = iota
	alignCenter
	alignRight
)

type chatMessage struct {
	role    string
	content string
}

type Model struct {
	input       textinput.Model
	viewport    viewport.Model
	spinner     spinner.Model
	agent       *agent.Agent
	session     *session.Session
	messages    []chatMessage
	busy        bool
	width       int
	height      int
	ready       bool

	// pending 人机协作暂停状态
	pending        *agent.PendingQueue
	currentPending int // 当前正在审核的 item 索引
	approvals      []bool // 累积的用户决策（每个 item 是否允许）
}

// agentResultMsg 是 Agent 执行后返回给 TUI 的消息。
// 有三种可能：answer 完成 / pending 需要用户确认 / err 错误。
type agentResultMsg struct {
	answer  string
	pending *agent.PendingQueue
	err     error
}

// agentResumedMsg 是 ResumeAgent 返回后的消息（结构和 agentResultMsg 一致）。
type agentResumedMsg agentResultMsg

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
		answer, pending, err := a.RunAgent(ctx, s, prompt)
		return agentResultMsg{answer: answer, pending: pending, err: err}
	}
}

// resumeAgentCmd 在用户确认后恢复 Agent 执行。
func resumeAgentCmd(a *agent.Agent, s *session.Session, pending *agent.PendingQueue, approved []bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), agentTimeout)
		defer cancel()
		answer, newPending, err := a.ResumeAgent(ctx, s, pending, approved)
		return agentResumedMsg{answer: answer, pending: newPending, err: err}
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
		// 如果正在等用户确认，优先处理 confirm 按键
		if m.pending != nil {
			return m.handleConfirmKeys(msg)
		}

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
		return m.handleAgentResult(msg)

	case agentResumedMsg:
		// ResumeAgent 返回的消息，和 agentResultMsg 结构一样
		return m.handleAgentResult(agentResultMsg(msg))
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleAgentResult 处理 Agent 的返回（完成 / pending / 错误 三种情况）。
func (m Model) handleAgentResult(msg agentResultMsg) (tea.Model, tea.Cmd) {
	m.busy = false

	// 有 pending → 暂停，进入人机协作模式
	if msg.pending != nil && len(msg.pending.Items) > 0 {
		m.pending = msg.pending
		m.currentPending = 0
		m.approvals = nil // 每次新的 pause 重置
		return m, nil
	}

	// 恢复执行后 pending 变 nil → 清除状态
	if m.pending != nil {
		m.pending = nil
		m.currentPending = 0
		m.approvals = nil
	}

	// 正常完成 / 错误
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

// handleConfirmKeys 处理人机协作弹框里的用户按键。
// 支持三种快捷决策：
//   y → 允许当前 item，r → 拒绝当前 item，a → 允许全部，n → 拒绝全部
func (m Model) handleConfirmKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil || m.currentPending >= len(m.pending.Items) {
		m.pending = nil
		return m, nil
	}

	switch strings.ToLower(msg.String()) {
	case "y":
		// 允许当前，继续下一个
		return m.confirmCurrent(true, true)
	case "r":
		// 拒绝当前，继续下一个
		return m.confirmCurrent(false, true)
	case "a":
		// 允许全部剩余
		return m.confirmAll(true)
	case "n":
		// 拒绝全部剩余
		return m.confirmAll(false)
	case "q":
		// 退出所有待确认（拒绝剩余）
		return m.confirmAll(false)
	}

	return m, nil
}

// confirmCurrent 处理单个 pending item 的决策（允许或拒绝）。
// 先记录决策到 approvals 数组，然后判断是否所有 item 都审核完了。
func (m Model) confirmCurrent(approved bool, advance bool) (tea.Model, tea.Cmd) {
	item := m.pending.Items[m.currentPending]
	m.approvals = append(m.approvals, approved)

	action := "✅ 允许"
	if !approved {
		action = "❌ 拒绝"
	}
	m.messages = append(m.messages, chatMessage{
		role:    "system",
		content: fmt.Sprintf("%s: %s（%s · %s）", action, item.ToolName, item.Decision.RiskLevel, item.Decision.Reason),
	})

	if advance {
		m.currentPending++
		if m.currentPending >= len(m.pending.Items) {
			// 所有 item 都审核完了 → 恢复 Agent
			m.busy = true // ResumeAgent 可能要花点时间，显示 spinner
			return m, tea.Batch(
				resumeAgentCmd(m.agent, m.session, m.pending, m.approvals),
				m.spinner.Tick,
			)
		}
	}

	m.refreshViewport()
	return m, nil
}

// confirmAll 一次性决定所有剩余 pending items。
func (m Model) confirmAll(approved bool) (tea.Model, tea.Cmd) {
	for m.currentPending < len(m.pending.Items) {
		m.approvals = append(m.approvals, approved)
		item := m.pending.Items[m.currentPending]
		action := "✅ 允许"
		if !approved {
			action = "❌ 拒绝"
		}
		m.messages = append(m.messages, chatMessage{
			role:    "system",
			content: fmt.Sprintf("%s: %s", action, item.ToolName),
		})
		m.currentPending++
	}
	m.busy = true
	m.refreshViewport()
	return m, tea.Batch(
		resumeAgentCmd(m.agent, m.session, m.pending, m.approvals),
		m.spinner.Tick,
	)
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
// 支持：代码块、标题、分隔线、引用、表格、加粗、斜体、行内代码、链接，以及行内格式嵌套。
func renderMarkdown(md string) string {
	blocks := parseBlocks(md)
	var b strings.Builder
	for _, block := range blocks {
		switch block.kind {
		case "code":
			renderCodeBlock(&b, block)
		case "table":
			renderTableBlock(&b, block)
		default:
			renderNormalBlock(&b, block)
		}
	}
	return b.String()
}

// parseBlocks 把 Markdown 文本切分成独立的块（代码块、表格块、普通行）。
func parseBlocks(md string) []mdBlock {
	lines := strings.Split(md, "\n")
	var blocks []mdBlock
	var current []string
	inCode := false
	codeLang := ""

	flushNormal := func() {
		if len(current) > 0 {
			// 检查 current 是否构成一个表格块
			if tb, ok := tryParseTable(current); ok {
				blocks = append(blocks, mdBlock{kind: "table", lines: tb})
			} else {
				blocks = append(blocks, mdBlock{kind: "normal", lines: current})
			}
			current = nil
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")

		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				// 进入代码块：先 flush 之前的普通块
				flushNormal()
				inCode = true
				codeLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			} else {
				// 退出代码块
				blocks = append(blocks, mdBlock{
					kind:  "code",
					lines: current,
					lang:  codeLang,
				})
				current = nil
				codeLang = ""
				inCode = false
			}
			continue
		}

		if inCode {
			current = append(current, trimmed)
			continue
		}

		// 非代码块：检查是否需要 flush
		if len(current) > 0 {
			// 当前行不是 | 开头 且 current 里有 | 行 → 可能是表格块结束
			if !strings.HasPrefix(strings.TrimSpace(trimmed), "|") {
				flushNormal()
			}
		}
		current = append(current, trimmed)
	}

	// 收尾
	if inCode {
		blocks = append(blocks, mdBlock{kind: "code", lines: current, lang: codeLang})
	} else {
		flushNormal()
	}

	return blocks
}

// tryParseTable 尝试把一组连续行解析为 Markdown 表格，成功时返回规范化的表格行和 true。
// 表格要求：至少 2 行，第一行表头 + 第二行分隔行（| --- | --- | 或带对齐标记）。
func tryParseTable(lines []string) ([]string, bool) {
	if len(lines) < 2 {
		return nil, false
	}

	// 检查第二行是否为分隔行
	if !tableSepRe.MatchString(lines[1]) {
		return nil, false
	}

	// 检查第一行和至少一个数据行是否像表格行
	headerOK := tableRowRe.MatchString(lines[0])
	dataOK := false
	for i := 2; i < len(lines); i++ {
		if tableRowRe.MatchString(lines[i]) {
			dataOK = true
			break
		}
	}

	// 允许只有表头+分隔行（空表格），或者表头+分隔行+数据行
	if !headerOK {
		return nil, false
	}
	_ = dataOK // dataOK 目前只用于日志判断，保留以便扩展

	// 过滤掉非表格行（末尾可能有不属于表格的空行或其他行，由 parseBlocks 负责边界隔离）
	var tableLines []string
	for _, l := range lines {
		if tableRowRe.MatchString(l) || tableSepRe.MatchString(l) {
			tableLines = append(tableLines, l)
		} else if strings.TrimSpace(l) == "" {
			// 遇到空行停止
			break
		} else {
			// 遇到非表格非空行停止
			break
		}
	}

	if len(tableLines) < 2 {
		return nil, false
	}

	return tableLines, true
}

// renderCodeBlock 渲染代码块。
func renderCodeBlock(b *strings.Builder, block mdBlock) {
	b.WriteString("\n")
	for _, line := range block.lines {
		b.WriteString(mdCodeBlock.Render(line))
		b.WriteString("\n")
	}
}

// renderNormalBlock 渲染普通段落（标题、分隔线、引用、普通文本）。
func renderNormalBlock(b *strings.Builder, block mdBlock) {
	for _, line := range block.lines {
		t := strings.TrimRight(line, " \t")
		switch {
		case headingRe.MatchString(t):
			b.WriteString(mdHeading.Render(t))
		case hrRe.MatchString(t):
			b.WriteString(strings.Repeat("─", 40))
		case strings.HasPrefix(t, ">"):
			b.WriteString(mdQuote.Render(strings.TrimSpace(strings.TrimPrefix(t, ">"))))
		case strings.TrimSpace(t) == "":
			// 空行仅换行
		default:
			b.WriteString(renderInline(t))
		}
		b.WriteString("\n")
	}
}

// parseTableCells 从一行表格文本中提取单元格内容（去除首尾 | 并 trim 空格）。
func parseTableCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// parseTableAlign 解析分隔行每列的对齐方式。
func parseTableAlign(sepLine string, colCount int) []tableAlign {
	cells := parseTableCells(sepLine)
	aligns := make([]tableAlign, colCount)
	for i := 0; i < colCount; i++ {
		if i >= len(cells) {
			break
		}
		c := strings.TrimSpace(cells[i])
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		switch {
		case left && right:
			aligns[i] = alignCenter
		case right:
			aligns[i] = alignRight
		default:
			aligns[i] = alignLeft
		}
	}
	return aligns
}

// renderTableBlock 渲染一个 Markdown 表格块为对齐的 ASCII 表格。
func renderTableBlock(b *strings.Builder, block mdBlock) {
	if len(block.lines) < 2 {
		return
	}

	header := parseTableCells(block.lines[0])
	colCount := len(header)
	if colCount == 0 {
		return
	}

	// 解析对齐方式
	aligns := parseTableAlign(block.lines[1], colCount)

	// 解析数据行（跳过第 1 行分隔行）
	var dataRows [][]string
	for i := 2; i < len(block.lines); i++ {
		if !tableRowRe.MatchString(block.lines[i]) {
			continue
		}
		cells := parseTableCells(block.lines[i])
		// 补齐或截断到 colCount
		for len(cells) < colCount {
			cells = append(cells, "")
		}
		if len(cells) > colCount {
			cells = cells[:colCount]
		}
		dataRows = append(dataRows, cells)
	}

	// 计算每列最大可见宽度（用纯文本，不含 ANSI 码）
	colWidths := make([]int, colCount)
	for i, h := range header {
		colWidths[i] = runeLen(h)
	}
	for _, row := range dataRows {
		for i, cell := range row {
			if w := runeLen(cell); w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}

	// 每列至少宽 3（确保 --- 分隔线能显示）
	for i := range colWidths {
		if colWidths[i] < 3 {
			colWidths[i] = 3
		}
	}

	// 渲染表头行
	b.WriteString(renderTableRow(header, colWidths, aligns))

	// 渲染分隔行
	b.WriteString(renderTableSep(colWidths, aligns))

	// 渲染数据行
	for _, row := range dataRows {
		b.WriteString(renderTableRow(row, colWidths, aligns))
	}
}

// runeLen 计算字符串的可见字符宽度（按 rune 计数，兼容中文）。
func runeLen(s string) int {
	return len([]rune(s))
}

// renderTableRow 渲染一行表格（对单元格内容做 inline 格式渲染，列对齐）。
func renderTableRow(cells []string, widths []int, aligns []tableAlign) string {
	// 先渲染每个单元格的 inline 格式，得到带 ANSI 的文本
	rendered := make([]string, len(cells))
	for i, cell := range cells {
		rendered[i] = renderInline(cell)
	}

	var b strings.Builder
	b.WriteString(mdTableBorder.Render("|"))
	for i := range widths {
		// 用纯文本算填充量
		pureLen := runeLen(cells[i])
		pad := widths[i] - pureLen
		leftPad, rightPad := 0, 0
		switch aligns[i] {
		case alignRight:
			leftPad, rightPad = pad, 0
		case alignCenter:
			leftPad = pad / 2
			rightPad = pad - leftPad
		default:
			leftPad, rightPad = 0, pad
		}
		b.WriteString(strings.Repeat(" ", leftPad))
		b.WriteString(rendered[i])
		b.WriteString(strings.Repeat(" ", rightPad))
		b.WriteString(mdTableBorder.Render("|"))
	}
	b.WriteString("\n")
	return b.String()
}

// renderTableSep 渲染表格的分隔线（带对齐标记）。
func renderTableSep(widths []int, aligns []tableAlign) string {
	var b strings.Builder
	b.WriteString(mdTableBorder.Render("|"))
	for i := range widths {
		w := widths[i]
		left, right := false, false
		if i < len(aligns) {
			left = aligns[i] == alignLeft || aligns[i] == alignCenter
			right = aligns[i] == alignRight || aligns[i] == alignCenter
		}
		dashes := strings.Repeat("-", w)
		if left {
			dashes = ":" + dashes[1:]
		}
		if right {
			dashes = dashes[:len(dashes)-1] + ":"
		}
		b.WriteString(mdTableBorder.Render(dashes))
		b.WriteString(mdTableBorder.Render("|"))
	}
	b.WriteString("\n")
	return b.String()
}

// renderInline 处理行内格式：先占位行内代码，再逐层处理粗体/斜体/链接，最后还原占位。
// 这样可以正确处理嵌套格式，例如 **bold with `code` inside**。
func renderInline(s string) string {
	const placeholderPrefix = "\x00INLINE_CODE_"
	const placeholderSuffix = "\x00"

	// 第一阶段：把所有行内代码替换为占位符
	var codeStore []string
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1] // 去掉反引号
		idx := len(codeStore)
		codeStore = append(codeStore, inner)
		return fmt.Sprintf("%s%d%s", placeholderPrefix, idx, placeholderSuffix)
	})

	// 第二阶段：处理粗体 **...**（内容里可能含占位符）
	s = inlineBoldRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-2]
		return mdBold.Render(inner)
	})

	// 第三阶段：处理斜体 *...*（此时粗体已去掉 ** 标记，不会误匹配）
	s = inlineItalicRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		return mdItalic.Render(inner)
	})

	// 第四阶段：处理链接 [text](url)
	s = inlineLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		// 找到 ] 和 ( 的分界
		closeBracket := strings.Index(m, "]")
		if closeBracket < 0 {
			return m
		}
		text := m[1:closeBracket]
		return mdLink.Render(text)
	})

	// 第五阶段：还原行内代码占位符（应用行内代码样式）
	for i, code := range codeStore {
		placeholder := fmt.Sprintf("%s%d%s", placeholderPrefix, i, placeholderSuffix)
		s = strings.ReplaceAll(s, placeholder, mdInlineCode.Render(code))
	}

	return s
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

		if m.pending != nil && m.currentPending < len(m.pending.Items) {
			// 人机协作弹框模式
			status = confirmStatusBar(m)
			content = m.viewport.View() + "\n" + confirmBox(m) + "\n" + status
		} else if m.busy {
			status = m.spinner.View() + " 思考中…（模型可能正在调用工具）"
			content = m.viewport.View() + "\n" + status + "\n" + m.input.View()
		} else {
			content = m.viewport.View() + "\n" + status + "\n" + m.input.View()
		}
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// confirmBox 渲染人机协作弹框。
func confirmBox(m Model) string {
	if m.pending == nil || m.currentPending >= len(m.pending.Items) {
		return ""
	}

	item := m.pending.Items[m.currentPending]
	total := len(m.pending.Items)

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("┌──────── 🔔 需要你确认 ────────\n")
	b.WriteString(fmt.Sprintf("│ 第 %d / %d 项\n", m.currentPending+1, total))
	b.WriteString(fmt.Sprintf("│ 工具: %s\n", item.ToolName))
	b.WriteString(fmt.Sprintf("│ 风险: %s  %s\n", item.Decision.RiskLevel, item.Decision.Reason))

	// 展示参数（truncate 超长）
	if item.ArgsJSON != "" && len(item.ArgsJSON) < 200 {
		b.WriteString(fmt.Sprintf("│ 参数: %s\n", item.ArgsJSON))
	} else if item.ArgsJSON != "" {
		b.WriteString(fmt.Sprintf("│ 参数: %s...\n", item.ArgsJSON[:190]))
	}

	b.WriteString("│\n")
	b.WriteString("│ [Y] 允许  [R] 拒绝  [A] 全部允许  [N] 全部拒绝\n")
	b.WriteString("└───────────────────────────────\n")

	return b.String()
}

// confirmStatusBar 是人机协作模式下的底部状态栏。
func confirmStatusBar(m Model) string {
	if m.pending == nil || m.currentPending >= len(m.pending.Items) {
		return ""
	}
	item := m.pending.Items[m.currentPending]
	return fmt.Sprintf("等待决策: %s · %s  [Y]允许 [R]拒绝 [A]全允许 [N]全拒绝",
		item.ToolName, item.Decision.RiskLevel)
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
