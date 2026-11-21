package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager 管理多个 MCP 服务端的连接和生命周期。
//
// Manager 同时维护每个 server 的连接状态（alive / dead）和工具注册信息。
// 当某个 server 挂了（连接级故障），CallTool 会自动 MarkDead，
// Agent 在每次 RunAgent 开头会调用 TryReconnectAll 尝试恢复。
//
// 同时追踪每个 alive server 是否需要刷新工具列表（pendingRefresh），
// 用于检测 server 部署新版后工具列表/参数 schema 的变化。
type Manager struct {
	sessions    map[string]*sdkmcp.ClientSession
	config      *Config                   // 完整配置，重连时使用
	serverAlive map[string]bool           // serverName → alive（true）/ dead（false）
	serverTools map[string][]*sdkmcp.Tool // serverName → 该 server 注册的原始工具结构

	// 刷新标记：CallTool 返回 IsError=true 时置 true，
	// AliveServerSync 刷新后清 false。
	pendingRefresh map[string]bool

	// IsError 连续计数器：server → 工具名 → 连续 IsError 次数。
	// 用于检测 server 行为变化（工具被删、schema 变了等）。
	toolErrorStreak map[string]map[string]int
}

// NewManager 创建 MCP 连接管理器。
func NewManager() *Manager {
	return &Manager{
		sessions:        make(map[string]*sdkmcp.ClientSession),
		serverAlive:     make(map[string]bool),
		serverTools:     make(map[string][]*sdkmcp.Tool),
		pendingRefresh:  make(map[string]bool),
		toolErrorStreak: make(map[string]map[string]int),
	}
}

// ConnectAll 根据配置启动 MCP 服务端并建立连接。
//
// 每个 server 独立尝试，成功的标记为 alive，失败的标记为 dead。
// 不会因为某个 server 连接失败而阻塞其他 server。
func (m *Manager) ConnectAll(ctx context.Context, config *Config) []error {
	m.config = config // 保存配置供后续重连使用

	var errs []error

	for name, server := range config.MCPServers {
		if _, exists := m.sessions[name]; exists {
			continue
		}

		session, err := connectOne(ctx, name, server)
		if err != nil {
			errs = append(errs, fmt.Errorf("服务 %q 连接失败: %w", name, err))
			m.serverAlive[name] = false
			continue
		}

		m.sessions[name] = session
		m.serverAlive[name] = true
	}

	return errs
}

// RegisterServerTools 注册某个 server 的工具列表。
// Agent 在 NewAgent 里 ListTools 成功后调用，让 Manager 知道每个 server 提供了哪些工具。
func (m *Manager) RegisterServerTools(serverName string, tools []*sdkmcp.Tool) {
	m.serverTools[serverName] = tools
}

// ServerTools 返回指定 server 注册的原始工具列表。
// Agent 在 refreshTools 时用它来重新构建 MCP 工具。
func (m *Manager) ServerTools(serverName string) ([]*sdkmcp.Tool, bool) {
	tools, ok := m.serverTools[serverName]
	return tools, ok
}

// IsAlive 返回指定 server 当前是否 alive。
// 未知的 server 名视为 dead（没有连接过的 server 也不应该出现在 alive 里）。
func (m *Manager) IsAlive(serverName string) bool {
	return m.serverAlive[serverName]
}

// AliveServers 返回所有当前 alive 的 server 名称。
func (m *Manager) AliveServers() []string {
	var names []string
	for name, alive := range m.serverAlive {
		if alive {
			names = append(names, name)
		}
	}
	return names
}

// DeadServers 返回所有当前 dead 的 server 名称。
func (m *Manager) DeadServers() []string {
	var names []string
	for name, alive := range m.serverAlive {
		if !alive {
			names = append(names, name)
		}
	}
	return names
}

// closeSessionWithTimeout 在独立 goroutine 里 Close session，给 5s 超时。
//
// session.Close 底层可能阻塞（比如 SSE 的 HTTP 连接还挂着没法优雅关闭，
// 或者 stdio 子进程退出了但 stdout 读端还卡在 EOF 等待）。
// 我们不应该让这个阻塞住主流程，所以用 goroutine + select 超时兜底。
func closeSessionWithTimeout(session *sdkmcp.ClientSession, name string) error {
	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		fmt.Fprintf(os.Stderr, "ℹ️  MCP 服务 %q 的 Close 超过 5s 未返回（可能已阻塞，继续清理）\n", name)
		return fmt.Errorf("close timeout")
	}
}

// MarkDead 把指定 server 标记为 dead，关闭其 session。
// CallTool 检测到连接级故障时自动调用；Agent 也可以显式调用。
func (m *Manager) MarkDead(serverName string) {
	if !m.serverAlive[serverName] {
		return // 已经是 dead，不重复处理
	}

	if session, ok := m.sessions[serverName]; ok {
		if err := closeSessionWithTimeout(session, serverName); err != nil {
			// Close 失败没关系，底层 transport 可能早就断了
			fmt.Fprintf(os.Stderr, "ℹ️  关闭 MCP 服务 %q 时遇到错误（可忽略）: %v\n", serverName, err)
		}
		delete(m.sessions, serverName)
	}

	m.serverAlive[serverName] = false
	// dead 的 server 不再需要刷新标记和错误追踪
	delete(m.pendingRefresh, serverName)
	delete(m.toolErrorStreak, serverName)

	fmt.Fprintf(os.Stderr, "⚠️  MCP 服务 %q 已断开，已从工具列表移除\n", serverName)
}

// TryReconnect 尝试重连单个 dead server。成功时标记 alive 并刷新工具列表。
// 如果 server 已经 alive，返回 nil 不做任何事。
func (m *Manager) TryReconnect(ctx context.Context, name string) error {
	if m.serverAlive[name] {
		return nil // 已经 alive，不需要重连
	}

	if m.config == nil {
		return fmt.Errorf("Manager 没有配置，无法重连 %q", name)
	}
	server, ok := m.config.MCPServers[name]
	if !ok {
		return fmt.Errorf("配置里没有 server %q", name)
	}

	session, err := connectOne(ctx, name, server)
	if err != nil {
		return fmt.Errorf("重连失败: %w", err)
	}

	// 重连成功：标记 alive + 存 session + 重新拉工具列表
	m.sessions[name] = session
	m.serverAlive[name] = true

	// 重连后 server 可能部署了新版本，工具列表可能变了 → 重新 ListTools
	toolsCtx, cancel := context.WithTimeout(ctx, server.GetTimeout())
	defer cancel()
	newTools, err := session.ListTools(toolsCtx, &sdkmcp.ListToolsParams{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  MCP 服务 %q 重连成功但拉工具列表失败: %v\n", name, err)
		// 仍然 alive，只是工具列表保持旧的
	} else {
		m.serverTools[name] = newTools.Tools
	}

	fmt.Fprintf(os.Stderr, "✅ MCP 服务 %q 已重新连接\n", name)
	return nil
}

// TryReconnectAll 尝试重连所有 dead server，返回成功重连的 server 名列表。
// Agent 每次 RunAgent 开头调用。
func (m *Manager) TryReconnectAll(ctx context.Context) []string {
	var connected []string
	for name, alive := range m.serverAlive {
		if alive {
			continue
		}
		if err := m.TryReconnect(ctx, name); err != nil {
			fmt.Fprintf(os.Stderr, "ℹ️  MCP 服务 %q 重连失败（继续尝试）: %v\n", name, err)
			continue
		}
		connected = append(connected, name)
	}
	return connected
}

// connectOne 连接单个 MCP Server，带 per-server 独立超时和重试。
func connectOne(ctx context.Context, name string, server ServerConfig) (*sdkmcp.ClientSession, error) {
	client := sdkmcp.NewClient(
		&sdkmcp.Implementation{
			Name:    "devagent",
			Version: "1.0.0",
		},
		nil,
	)

	timeout := server.GetTimeout()
	maxAttempts := server.Retry + 1 // Retry=N → 总共 N+1 次尝试

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// transport 必须在每次尝试时重新构造：stdio 的 CommandTransport.Connect
		// 内部会执行 Cmd.Start()，复用同一个实例时第二次必然报
		// "exec: already started"，导致重试配置完全失效。
		transport, err := buildTransport(server)
		if err != nil {
			return nil, fmt.Errorf("构造传输层失败: %w", err)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		session, err := client.Connect(attemptCtx, transport, nil)
		cancel()

		if err == nil {
			return session, nil
		}

		lastErr = err
		if attempt < maxAttempts {
			// 指数退避：50ms → 100ms → 200ms
			backoff := time.Duration(50*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}

	return nil, fmt.Errorf("共尝试 %d 次仍失败: %w", maxAttempts, lastErr)
}

// buildTransport 根据 ServerConfig 构造对应的 Transport 实现。
func buildTransport(server ServerConfig) (sdkmcp.Transport, error) {
	switch server.GetTransport() {
	case TransportStdio:
		return buildStdioTransport(server)
	case TransportSSE:
		return buildSSETransport(server)
	case TransportStreamable:
		return buildStreamableTransport(server)
	default:
		return nil, fmt.Errorf("未知传输类型: %s", server.GetTransport())
	}
}

// buildStdioTransport 构造子进程 stdio 传输层。
func buildStdioTransport(server ServerConfig) (sdkmcp.Transport, error) {
	cmd := exec.Command(server.Command, server.Args...)

	if len(server.Env) > 0 {
		cmd.Env = os.Environ()
		for key, value := range server.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}

	return &sdkmcp.CommandTransport{Command: cmd}, nil
}

// headerTransport 给所有 HTTP 请求注入自定义 Header。
// go-sdk 的 SSEClientTransport 不支持直接配置 Header，
// 需要通过自定义 http.Client.Transport 注入（常用于 Authorization）。
type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

// RoundTrip 实现 http.RoundTripper，向请求注入配置的 Header。
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	for key, values := range t.headers {
		// 直接赋值而不是 Set：保留多值 Header，同时复制切片避免共享底层数组
		cloned.Header[key] = append([]string(nil), values...)
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(cloned)
}

// buildSSETransport 构造 HTTP + SSE 传输层。
func buildSSETransport(server ServerConfig) (sdkmcp.Transport, error) {
	if strings.TrimSpace(server.URL) == "" {
		return nil, fmt.Errorf("sse 传输需要配置 url")
	}

	httpClient := &http.Client{}

	// 配置了 headers 时，用自定义 RoundTripper 注入
	if len(server.Headers) > 0 {
		headers := http.Header{}
		for key, value := range server.Headers {
			headers.Set(key, value)
		}
		httpClient.Transport = &headerTransport{headers: headers}
	}

	return &sdkmcp.SSEClientTransport{
		Endpoint:   server.URL,
		HTTPClient: httpClient,
	}, nil
}

// buildStreamableTransport 构造 Streamable HTTP 传输层（MCP 2025-03 规范）。
// 与传统 SSE 的区别：单一 endpoint，POST 请求响应可以是 JSON 或 SSE 流，
// 不再需要先挂 SSE 长连接获取独立的消息 endpoint。
// GitHub 官方远程 MCP 服务（api.githubcopilot.com/mcp/）即使用此协议。
func buildStreamableTransport(server ServerConfig) (sdkmcp.Transport, error) {
	if strings.TrimSpace(server.URL) == "" {
		return nil, fmt.Errorf("streamable 传输需要配置 url")
	}

	httpClient := &http.Client{}

	// 配置了 headers 时，复用 headerTransport 注入（常用于 Authorization）
	if len(server.Headers) > 0 {
		headers := http.Header{}
		for key, value := range server.Headers {
			headers.Set(key, value)
		}
		httpClient.Transport = &headerTransport{headers: headers}
	}

	return &sdkmcp.StreamableClientTransport{
		Endpoint:   server.URL,
		HTTPClient: httpClient,
	}, nil
}

// ListTools 获取指定 MCP 服务端提供的工具列表。
// 带 per-server 独立超时（来自 config.CallTimeout，默认 60s）。
func (m *Manager) ListTools(
	ctx context.Context,
	serverName string,
) ([]*sdkmcp.Tool, error) {
	session, exists := m.sessions[serverName]
	if !exists {
		return nil, fmt.Errorf("MCP 服务 %q 尚未连接", serverName)
	}

	callTimeout := 60 * time.Second
	if m.config != nil {
		if server, ok := m.config.MCPServers[serverName]; ok {
			callTimeout = server.GetCallTimeout()
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	result, err := session.ListTools(callCtx, &sdkmcp.ListToolsParams{})
	if err != nil {
		if isConnectionError(err) {
			m.MarkDead(serverName)
		}
		return nil, fmt.Errorf("获取 MCP 工具列表失败: %w", err)
	}

	return result.Tools, nil
}

// AliveServerSync 对账：对所有 alive server 调 ListTools，和缓存的 serverTools 比对。
//
// 当 server 部署了新版（加了/删了工具、改了 schema）时，缓存的 serverTools 会过时。
// 此方法在每次 RunAgent 开头 TryReconnectAll 之后调用，保证 alive server 的工具列表与 server 实际一致。
//
// 返回有变化的 server 名列表，Agent 据此决定是否 refreshTools。
func (m *Manager) AliveServerSync(ctx context.Context) []string {
	var changed []string

	for _, name := range m.AliveServers() {
		session, ok := m.sessions[name]
		if !ok {
			continue
		}

		// 如果 server 没有 pendingRefresh 标记，跳过低频对账
		// 只有 CallTool 返回 IsError=true 时才会置 pendingRefresh=true
		// 这样避免每次 RunAgent 都多打一轮 ListTools RPC
		// （如果后面要加定时 ping，可以改成每次都对账或加时间窗口）
		if !m.pendingRefresh[name] {
			continue
		}

		server, srvOk := m.config.MCPServers[name]
		if !srvOk {
			continue
		}

		toolsCtx, cancel := context.WithTimeout(ctx, server.GetTimeout())
		newTools, err := session.ListTools(toolsCtx, &sdkmcp.ListToolsParams{})
		cancel()

		if err != nil {
			if isConnectionError(err) {
				m.MarkDead(name)
			} else {
				fmt.Fprintf(os.Stderr, "⚠️  MCP 服务 %q 对账失败（暂时保留旧工具列表）: %v\n", name, err)
			}
			continue
		}

		// 比对新旧工具列表：只看工具名集合是否变化
		oldSet := m.makeToolNameSet(m.serverTools[name])
		newSet := m.makeToolNameSet(newTools.Tools)

		if !equalToolSets(oldSet, newSet) {
			// 打印具体变化
			added, removed := diffToolSets(oldSet, newSet)
			if len(removed) > 0 {
				fmt.Fprintf(os.Stderr, "ℹ️  MCP 服务 %q 工具列表变化（移除了 %s）\n", name, strings.Join(removed, ", "))
			}
			if len(added) > 0 {
				fmt.Fprintf(os.Stderr, "ℹ️  MCP 服务 %q 工具列表变化（新增了 %s）\n", name, strings.Join(added, ", "))
			}
			m.serverTools[name] = newTools.Tools
			changed = append(changed, name)
		} else {
			// 工具名没变 → 但 schema 可能变了 → 也更新一份（零成本）
			m.serverTools[name] = newTools.Tools
			changed = append(changed, name)
		}

		// 清除刷新标记
		m.pendingRefresh[name] = false
		// 工具列表刷新后，连续 IsError 计数归零（新一轮开始）
		delete(m.toolErrorStreak, name)
	}

	return changed
}

// makeToolNameSet 把工具列表转成 set 方便比对。
func (m *Manager) makeToolNameSet(tools []*sdkmcp.Tool) map[string]bool {
	set := make(map[string]bool)
	for _, t := range tools {
		set[t.Name] = true
	}
	return set
}

// equalToolSets 判断两个工具名集合是否相同。
func equalToolSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name := range a {
		if !b[name] {
			return false
		}
	}
	return true
}

// diffToolSets 返回 (新增工具名, 移除工具名)。
func diffToolSets(oldSet, newSet map[string]bool) (added, removed []string) {
	for name := range newSet {
		if !oldSet[name] {
			added = append(added, name)
		}
	}
	for name := range oldSet {
		if !newSet[name] {
			removed = append(removed, name)
		}
	}
	return added, removed
}

// Close 关闭所有 MCP 会话并清空状态。
// 每个 session.Close 都带 5s 超时，防止单个阻塞的 session 拖住整个进程退出。
func (m *Manager) Close() {
	for name, session := range m.sessions {
		if err := closeSessionWithTimeout(session, name); err != nil {
			fmt.Fprintf(os.Stderr, "关闭 MCP 服务 %q 失败: %v\n", name, err)
		}
	}
	m.sessions = make(map[string]*sdkmcp.ClientSession)
	m.serverAlive = make(map[string]bool)
	m.serverTools = make(map[string][]*sdkmcp.Tool)
	m.pendingRefresh = make(map[string]bool)
	m.toolErrorStreak = make(map[string]map[string]int)
}

// CallTool 调用指定 MCP 服务端的工具。
//
// 带 per-server 独立超时（来自 config.CallTimeout，默认 60s），
// 防止模型调了一个慢工具导致整个 RunAgent 卡死。
//
// 故障分级处理：
//   - 连接级故障（process exit / pipe broken / EOF / context deadline 等）
//     → 自动 MarkDead(serverName)
//   - 业务级 IsError=true（工具不存在 / schema 不匹配 / 执行失败）
//     → 标记 pendingRefresh（下次 AliveServerSync 对账时刷新工具列表）
//     → 追踪 toolErrorStreak，连续超过阈值时立即触发对账
func (m *Manager) CallTool(
	ctx context.Context,
	serverName string,
	toolName string,
	arguments any,
) (*sdkmcp.CallToolResult, error) {
	session, exists := m.sessions[serverName]
	if !exists {
		return nil, fmt.Errorf("MCP 服务 %q 尚未连接", serverName)
	}

	// 从 config 拿 callTimeout，如果 config 没了兜底 60s
	callTimeout := 60 * time.Second
	if m.config != nil {
		if server, ok := m.config.MCPServers[serverName]; ok {
			callTimeout = server.GetCallTimeout()
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	params := &sdkmcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	}
	result, err := session.CallTool(callCtx, params)
	if err != nil {
		if isConnectionError(err) {
			m.MarkDead(serverName)
		}
		return nil, fmt.Errorf(
			"调用 MCP 工具 %q 失败: %w",
			toolName,
			err,
		)
	}

	// 业务级 IsError=true：标记 pendingRefresh，让下次 RunAgent 开头刷新工具列表
	if result.IsError {
		m.onToolError(serverName, toolName)
	}

	return result, nil
}

// onToolError 处理一次业务级 IsError=true 的调用。
// 追踪连续 IsError 次数，达到阈值时立即标记 pendingRefresh。
func (m *Manager) onToolError(serverName, toolName string) {
	streak, ok := m.toolErrorStreak[serverName]
	if !ok {
		streak = make(map[string]int)
		m.toolErrorStreak[serverName] = streak
	}

	streak[toolName]++

	// 同一个工具连续 IsError 超过 3 次 → server 行为大概率变了（被删/schema 变）
	// 标记 pendingRefresh，AliveServerSync 会在下次 RunAgent 开头对账
	if streak[toolName] >= 3 {
		m.pendingRefresh[serverName] = true
	}
}

// ExtractText 从 MCP 工具执行结果中提取文本。
func ExtractText(result *sdkmcp.CallToolResult) (string, error) {
	if result == nil {
		return "", fmt.Errorf("MCP 工具返回结果为空")
	}

	var output strings.Builder

	for _, item := range result.Content {
		switch content := item.(type) {
		case *sdkmcp.TextContent:
			output.WriteString(content.Text)
			output.WriteString("\n")

		case *sdkmcp.ImageContent:
			output.WriteString(
				fmt.Sprintf("[图片内容，类型: %s]\n", content.MIMEType),
			)

		default:
			output.WriteString("[暂不支持的内容类型]\n")
		}
	}

	text := output.String()

	// IsError 时把服务端返回的文本带进错误里，模型才能据此自纠
	// （例如工具不存在、参数不匹配、文件不存在）。
	if result.IsError {
		if detail := strings.TrimSpace(text); detail != "" {
			return "", fmt.Errorf("MCP 工具执行失败: %s", detail)
		}
		return "", fmt.Errorf("MCP 工具执行失败（服务端未返回错误详情）")
	}

	return text, nil
}

// isConnectionError 判断 error 是否属于「连接级故障」——
// 即 transport 已经断开，整个 server 不可用，应该 MarkDead。
//
// 排除业务级错误（工具不存在、参数错误、业务超时），那些不影响 transport 存活。
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}

	// io.EOF / io.ErrUnexpectedEOF 是典型的连接断开
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}

	msg := strings.ToLower(err.Error())

	// 这些关键词通常出现在 transport 已关闭、链路断开的场景下。
	// 这里刻意不写裸 "closed"：像 "input stream closed by server policy"
	// 这类业务级关闭提示会把整个 server 误判为 dead。
	connectionKeywords := []string{
		"eof",
		"unexpected eof",
		"connection closed",
		"session closed",
		"client closed",
		"closed by server",
		"broken pipe",
		"connection reset",
		"connection refused",
		"transport closed",
		"stream closed",
		"use of closed",
		"pipe closed",
		"process exited",
		"child process",
	}

	for _, kw := range connectionKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}

	return false
}
