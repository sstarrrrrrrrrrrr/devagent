package agent

import (
	"context"
	devmcp "devagent/mcp"
	"devagent/safety"
	"devagent/session"
	"devagent/tools"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

// 默认值。
const (
	defaultMaxToolRounds  = 10
	maxRepeatToolCalls    = 3 // 同一工具+相同参数连续调用超过此次数即视为循环
)

// Agent 负责协调大模型、工具和对话历史。
type Agent struct {
	client      openai.Client
	openAITools []responses.ToolUnionParam

	// 包装过的工具（SafetyGuard.Decide 已嵌入 Execute）。
	toolExecutors map[string]tools.Tool

	// 原始未包装的工具（用于 ResumeAgent 执行已确认的工具）。
	rawToolExecutors map[string]tools.Tool

	// 管理 MCP Server 连接。
	mcpManager *devmcp.Manager

	// safetyGuard 安全拦截器。
	safetyGuard *safety.SafetyGuard

	// MaxToolRounds 单次对话中工具调用的最大轮数。
	MaxToolRounds int
}

// NewAgent 创建 Agent，连接 MCP 并注册工具。
func NewAgent() (*Agent, error) {
	// 1. 获取 API Key
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("请先设置 DEEPSEEK_API_KEY 环境变量")
	}

	// 2. 创建 MCP Manager
	manager := devmcp.NewManager()

	// 如果初始化过程中出现错误，关闭已经建立的 MCP 连接
	initialized := false
	defer func() {
		if !initialized {
			manager.Close()
		}
	}()

	// 3. 加载安全策略（safety.json）
	safetyConfig, err := safety.LoadSafetyConfig("safety.json")
	if err != nil {
		return nil, fmt.Errorf("加载安全配置失败: %w", err)
	}
	workdir, _ := os.Getwd()
	safetyGuard, err := safety.NewSafetyGuard(safetyConfig, workdir)
	if err != nil {
		return nil, fmt.Errorf("创建安全拦截器失败: %w", err)
	}

	// 4. 加载 MCP 配置文件
	config, err := devmcp.LoadConfig("mcp.json")
	if err != nil {
		return nil, err
	}

	// 5. 连接所有 MCP Server
	ctx := context.Background()

	connectErrs := manager.ConnectAll(ctx, config)

	// 如果全部 server 都失败了，直接报错退出
	if len(connectErrs) == len(config.MCPServers) {
		parts := make([]string, len(connectErrs))
		for i, e := range connectErrs {
			parts[i] = e.Error()
		}
		return nil, fmt.Errorf("所有 MCP 服务均连接失败: %s", strings.Join(parts, "; "))
	}

	// 部分失败的情况：打印警告但继续，已连上的照常注册工具
	for _, e := range connectErrs {
		fmt.Fprintf(os.Stderr, "⚠️  %v\n", e)
	}

	// 6. 准备工具集合，先放入本地工具
	allTools := make([]tools.Tool, 0)

	allTools = append(allTools, tools.All...)

	// 7. 遍历所有 MCP Server，获取并转换工具（跳过没连上的）
	for serverName := range config.MCPServers {

		// 获取当前 Server 提供的工具
		mcpTools, err := manager.ListTools(ctx, serverName)
		if err != nil {
			// 这个 server 没连上，跳过继续
			fmt.Fprintf(os.Stderr, "⚠️  跳过 MCP 服务 %q: %v\n", serverName, err)
			continue
		}

		// 告诉 Manager 这个 server 注册了哪些原始工具（用于后续 refreshTools）
		manager.RegisterServerTools(serverName, mcpTools)

		// 转换成项目自己的 Tool 类型
		convertedTools, err := buildMCPTools(
			manager,
			serverName,
			mcpTools,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  MCP 服务 %q 工具转换失败: %v\n", serverName, err)
			continue
		}

		// MCP 工具默认 RiskLevel=L1Confirm（有副作用需要确认），
		// SafetyGuard.Decide() 再根据策略/参数动态升级或降级。
		for i := range convertedTools {
			convertedTools[i].RiskLevel = tools.L1Confirm
		}

		// 合并 MCP 工具
		allTools = append(allTools, convertedTools...)
	}

	// 8. 先保存原始未包装的工具 → rawToolExecutors（ResumeAgent 执行已确认工具时用）
	rawExecutors := make(map[string]tools.Tool, len(allTools))
	for _, t := range allTools {
		rawExecutors[t.Name] = t
	}

	// 9. 所有工具统一经过安全拦截器包装
	wrappedTools := safety.WrapAllTools(allTools, safetyGuard)

	// 10. 注册全部工具
	toolExecutors, openAITools, err := registerTools(wrappedTools)
	if err != nil {
		return nil, err
	}

	// 11. 创建 Agent
	agent := &Agent{
		client: openai.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL("https://api.deepseek.com"),
		),

		openAITools:      openAITools,
		toolExecutors:    toolExecutors,
		rawToolExecutors: rawExecutors,
		mcpManager:       manager,
		safetyGuard:      safetyGuard,
		MaxToolRounds:    defaultMaxToolRounds,
	}

	initialized = true

	return agent, nil
}

// effectiveMaxRounds 返回实际生效的最大轮数。
func (a *Agent) effectiveMaxRounds() int {
	if a.MaxToolRounds > 0 {
		return a.MaxToolRounds
	}
	return defaultMaxToolRounds
}

// refreshTools 根据 Manager 当前的 alive 状态重新构建工具列表。
//
// 每次 RunAgent 开头 TryReconnectAll 后调用，确保：
//   - 新重连成功的 server → 工具被加入 system prompt
//   - 已经 dead 的 server   → 工具从 toolExecutors / openAITools 里移除
//
// 重建的 MCP 工具统一经过安全拦截器包装（和 NewAgent 里的流程一致）。
func (a *Agent) refreshTools() {
	allTools := make([]tools.Tool, 0, len(tools.All)+16)

	// 本地工具永远可用
	allTools = append(allTools, tools.All...)

	// 从 Manager alive servers 里重新转换 MCP 工具
	for _, serverName := range a.mcpManager.AliveServers() {
		mcpTools, ok := a.mcpManager.ServerTools(serverName)
		if !ok || len(mcpTools) == 0 {
			continue
		}

		converted, err := buildMCPTools(a.mcpManager, serverName, mcpTools)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  刷新 MCP 服务 %q 的工具失败: %v\n", serverName, err)
			continue
		}

		// MCP 工具默认 RiskLevel=L1Confirm
		for i := range converted {
			converted[i].RiskLevel = tools.L1Confirm
		}
		allTools = append(allTools, converted...)
	}

	// 所有工具统一经过安全拦截器包装
	allTools = safety.WrapAllTools(allTools, a.safetyGuard)

	executors, openAITools, err := registerTools(allTools)
	if err != nil {
		// 理论上不会重名（registerTools 里有重名检查，但本地工具和 MCP 工具前缀不同）
		fmt.Fprintf(os.Stderr, "⚠️  工具注册时检测到重名: %v\n", err)
		return
	}

	a.toolExecutors = executors
	a.openAITools = openAITools
}

// hasDeadServers 判断 Manager 当前是否有任何 dead server。
// dead server 的工具应该从 toolExecutors / openAITools 里移除，
// 所以只要有 dead 的就需要 refreshTools。
func (a *Agent) hasDeadServers() bool {
	return len(a.mcpManager.DeadServers()) > 0
}

// RunAgent 执行用户任务，循环处理工具调用，直到得到最终回答。
//
// 返回值有三种可能：
//   - answer != "" + pending == nil   → 对话完成
//   - answer != "" + err != nil       → 对话完成但有错误需要展示
//   - pending != nil                  → 暂停，等用户确认 pending.Queue 里的工具调用
//                                      （调用方处理完后应调 ResumeAgent）
//
// 四层终止保护：
//  1. 系统提示词约束模型主动收口（见 instructions() 第 10 条）
//  2. 同一工具+相同参数连续调用超过 maxRepeatToolCalls 次 → 熔断
//  3. 总轮数达到 effectiveMaxRounds → 尝试提取模型已有的文字回答后返回
//  4. 上层 context 超时（由 tui 传入，默认 3min）
func (a *Agent) RunAgent(
	ctx context.Context,
	s *session.Session,
	prompt string,
) (string, *PendingQueue, error) {

	// 每次对话开始时，两阶段同步 MCP 状态
	connected := a.mcpManager.TryReconnectAll(ctx)
	synced := a.mcpManager.AliveServerSync(ctx)

	if len(connected) > 0 || len(synced) > 0 || a.hasDeadServers() {
		a.refreshTools()
	}

	s.History = append(s.History, message("user", prompt))

	params := a.buildModelParams(responses.ResponseNewParamsInputUnion{
		OfInputItemList: s.History,
	})
	resp, err := a.client.Responses.New(ctx, params)
	if err != nil {
		return "", nil, fmt.Errorf("调用模型失败: %w", err)
	}

	recentToolCalls := make(map[string]int)
	maxRounds := a.effectiveMaxRounds()

	for round := 1; round <= maxRounds; round++ {
		var toolCalls []responses.ResponseInputItemUnionParam
		var toolOutputs []responses.ResponseInputItemUnionParam
		var pendingItems []PendingItem

		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}

			sig := item.Name + "|" + item.Arguments.OfString
			recentToolCalls[sig]++
			if recentToolCalls[sig] >= maxRepeatToolCalls {
				return "", nil, fmt.Errorf(
					"检测到模型连续 %d 次以相同参数调用工具 %q，可能陷入循环，已停止执行",
					maxRepeatToolCalls, item.Name,
				)
			}

			result := runTool(ctx, item, a.toolExecutors, a.safetyGuard)

			if result.ToolName != "" {
				// 需要用户确认 → 收集到 pending 队列
				pendingItems = append(pendingItems, PendingItem{
					ToolName:  result.ToolName,
					ArgsJSON:  result.ArgsJSON,
					ArgsMap:   result.ArgsMap,
					Decision:  result.Decision,
					ToolDesc:  result.ToolDesc,
					ToolCall:  result.ToolCall,
				})
				// tool_call 先加进 History，tool_output 等用户决策后再加
				toolCalls = append(toolCalls, result.ToolCall)
				continue
			}

			// 正常执行完成
			toolCalls = append(toolCalls, result.ToolCall)
			toolOutputs = append(toolOutputs, result.ToolOutput)
		}

		if len(toolCalls) == 0 {
			answer := resp.OutputText()
			s.History = append(s.History, message("assistant", answer))
			return answer, nil, nil
		}

		// 有需要用户确认的工具 → 先暂停，不追加 History（ToolCall 加了，ToolOutput 没加）
		if len(pendingItems) > 0 {
			s.History = append(s.History, toolCalls...)
			return "", &PendingQueue{
				Items:  pendingItems,
				History: s.History,
			}, nil
		}

		s.History = append(s.History, toolCalls...)
		s.History = append(s.History, toolOutputs...)

		params = a.buildModelParams(responses.ResponseNewParamsInputUnion{
			OfInputItemList: s.History,
		})
		resp, err = a.client.Responses.New(ctx, params)
		if err != nil {
			return "", nil, fmt.Errorf("调用模型失败: %w", err)
		}
	}

	fallback := resp.OutputText()
	if strings.TrimSpace(fallback) != "" {
		s.History = append(s.History, message("assistant", fallback))
		return fallback, nil, fmt.Errorf(
			"达到最大工具调用轮数 %d，以上为模型已给出的部分回答",
			maxRounds,
		)
	}

	return "", nil, fmt.Errorf("超过最大工具调用轮数 %d，停止执行", maxRounds)
}

// PendingQueue 是需要用户确认的工具调用队列。
// Agent 返回给 TUI，TUI 弹框等用户逐个决策后调用 ResumeAgent。
type PendingQueue struct {
	Items []PendingItem
	// History 是当前 session 的 History 快照，ResumeAgent 会基于它继续。
	History []responses.ResponseInputItemUnionParam
}

// PendingItem 是队列里的单个需要确认的工具调用。
type PendingItem struct {
	ToolName string
	ArgsJSON string
	ArgsMap  map[string]any
	Decision safety.Decision
	ToolDesc string
	// ToolCall 是已追加进 History 的 function_call 记录。
	// 等用户决策后，ResumeAgent 会追加对应的 function_call_output。
	ToolCall responses.ResponseInputItemUnionParam
}

// ResumeAgent 从暂停状态恢复执行。
//
// approved 是用户对 pending 队列里每个 item 的决策：
//   - approved[i]=true  → 执行第 i 个工具调用
//   - approved[i]=false → 跳过第 i 个（tool_output 是 "用户拒绝了此操作"）
//
// 恢复后 Agent 继续模型循环，直到得到最终回答或再次遇到需要确认的工具。
func (a *Agent) ResumeAgent(
	ctx context.Context,
	s *session.Session,
	pending *PendingQueue,
	approved []bool,
) (string, *PendingQueue, error) {
	if pending == nil {
		return "", nil, fmt.Errorf("没有待恢复的 pending 队列")
	}

	// 1. 根据用户决策执行（或跳过）pending 里的每个 item
	var toolOutputs []responses.ResponseInputItemUnionParam
	for i, item := range pending.Items {
		decision := approved[i] // true=用户允许

		if decision {
			// 执行工具（ActionRequireConfirm 时 SafetyGuard 不会拦截，
			// 因为 runTool 里在 Execute 之前就 return 了 pending，
			// 这次直接调 tool.Execute 即可——但 Execute 里 SafetyGuard 还会再拦！
			// 所以这里需要用原始工具的 Execute，而不是被包装过的。
			// 简化处理：直接调 toolExecutors，SafetyGuard 会发现 ActionRequireConfirm
			// 但这里我们是在用户确认后手动调的——有问题。
			// 更好的方式是：SafetyGuard 包装的 Execute 加一个 "跳过确认" 参数。
			// 当前实现用一个 hack：包装过的 Execute 调完还是会返回 ErrRequireConfirm，
			// 因为 Decide 还是 RequireConfirm。所以用户确认后的执行需要另一条路径。
			// 暂时 hack：在 Agent 里存原始工具 map，确认后直接 Execute 原始版本。
			result, err := a.executeWithoutGuard(ctx, item.ToolName, item.ArgsMap)
			if err != nil {
				toolOutputs = append(toolOutputs, buildDirectOutput(item,
					fmt.Sprintf("工具执行失败: %v", err)))
			} else {
				toolOutputs = append(toolOutputs, buildDirectOutput(item, result))
			}
		} else {
			// 用户拒绝
			toolOutputs = append(toolOutputs, buildDirectOutput(item,
				fmt.Sprintf("用户拒绝了此操作，原因: %s（风险等级 %s）",
					item.Decision.Reason, item.Decision.RiskLevel.String())))
		}
	}

	// 2. 把 tool_outputs 追加进 History
	s.History = append(s.History, toolOutputs...)

	// 3. 继续模型循环（和 RunAgent 里的逻辑几乎一样，只是不重复同步 MCP 状态）
	params := a.buildModelParams(responses.ResponseNewParamsInputUnion{
		OfInputItemList: s.History,
	})
	resp, err := a.client.Responses.New(ctx, params)
	if err != nil {
		return "", nil, fmt.Errorf("调用模型失败: %w", err)
	}

	recentToolCalls := make(map[string]int)
	maxRounds := a.effectiveMaxRounds()

	for round := 1; round <= maxRounds; round++ {
		var toolCalls []responses.ResponseInputItemUnionParam
		var toolOutputs []responses.ResponseInputItemUnionParam
		var pendingItems []PendingItem

		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}

			sig := item.Name + "|" + item.Arguments.OfString
			recentToolCalls[sig]++
			if recentToolCalls[sig] >= maxRepeatToolCalls {
				return "", nil, fmt.Errorf(
					"检测到模型连续 %d 次以相同参数调用工具 %q，可能陷入循环",
					maxRepeatToolCalls, item.Name,
				)
			}

			result := runTool(ctx, item, a.toolExecutors, a.safetyGuard)

			if result.ToolName != "" {
				pendingItems = append(pendingItems, PendingItem{
					ToolName:  result.ToolName,
					ArgsJSON:  result.ArgsJSON,
					ArgsMap:   result.ArgsMap,
					Decision:  result.Decision,
					ToolDesc:  result.ToolDesc,
					ToolCall:  result.ToolCall,
				})
				toolCalls = append(toolCalls, result.ToolCall)
				continue
			}

			toolCalls = append(toolCalls, result.ToolCall)
			toolOutputs = append(toolOutputs, result.ToolOutput)
		}

		if len(toolCalls) == 0 {
			answer := resp.OutputText()
			s.History = append(s.History, message("assistant", answer))
			return answer, nil, nil
		}

		if len(pendingItems) > 0 {
			s.History = append(s.History, toolCalls...)
			return "", &PendingQueue{
				Items:  pendingItems,
				History: s.History,
			}, nil
		}

		s.History = append(s.History, toolCalls...)
		s.History = append(s.History, toolOutputs...)

		params = a.buildModelParams(responses.ResponseNewParamsInputUnion{
			OfInputItemList: s.History,
		})
		resp, err = a.client.Responses.New(ctx, params)
		if err != nil {
			return "", nil, fmt.Errorf("调用模型失败: %w", err)
		}
	}

	fallback := resp.OutputText()
	if strings.TrimSpace(fallback) != "" {
		s.History = append(s.History, message("assistant", fallback))
		return fallback, nil, fmt.Errorf("达到最大轮数，以上为模型已给出的部分回答")
	}

	return "", nil, fmt.Errorf("超过最大工具调用轮数 %d，停止执行", maxRounds)
}

// executeWithoutGuard 跳过 SafetyGuard 执行原始工具。
// 用于用户已确认 ActionRequireConfirm 的工具后恢复执行。
//
// 实现方式：注册工具时同时保存一份"原始未包装"的 tool map，
// executeWithoutGuard 优先从原始 map 取，没有就从 toolExecutors 取
// （取到的就是被包装过的，会再拦一次 ActionRequireConfirm —— 理论上不该发生）。
func (a *Agent) executeWithoutGuard(ctx context.Context, toolName string, args map[string]any) (string, error) {
	if a.rawToolExecutors != nil {
		if tool, exists := a.rawToolExecutors[toolName]; exists {
			return tool.Execute(ctx, args)
		}
	}
	// 没有原始版本就用包装过的（会再拦 ActionRequireConfirm，但用户已确认）
	// 这时 SafetyGuard.Decide 还是 RequireConfirm，所以必须走原始 map
	tool, exists := a.toolExecutors[toolName]
	if !exists {
		return "", fmt.Errorf("工具不存在: %s", toolName)
	}
	return tool.Execute(ctx, args)
}

// message 构造一条对话消息。
func message(role, text string) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{
		OfInputMessage: &responses.ResponseInputItemMessageParam{
			Role: role,
			Content: responses.ResponseInputMessageContentListParam{
				{OfInputText: &responses.ResponseInputTextParam{Text: text}},
			},
		},
	}
}

// buildModelParams 统一构造模型请求参数。
func (a *Agent) buildModelParams(
	input responses.ResponseNewParamsInputUnion,
) responses.ResponseNewParams {
	return responses.ResponseNewParams{
		Model:        "deepseek-flash",
		Instructions: openai.String(a.instructions()),
		Input:        input,
		Tools:        a.openAITools,
	}
}

// instructions 生成系统提示词，并动态列出当前注册的全部工具。
func (a *Agent) instructions() string {
	var b strings.Builder
	b.WriteString("你是小哈，一个由用户独立开发的 AI 编程助手，运行在终端交互界面中。" +
		"你的主要目标是帮助用户理解代码、分析项目结构、阅读文件和解决编程问题。")

	b.WriteString("\n\n一、身份与基本原则")
	b.WriteString("\n1. 当用户询问你的身份时，明确回答自己是小哈，是用户开发的 AI 编程助手。")
	b.WriteString("\n2. 始终使用中文回答，除非用户明确要求使用其他语言。")
	b.WriteString("\n3. 回答应准确、清晰、简洁，优先解决用户当前的问题，不要输出无关内容。")
	b.WriteString("\n4. 不要声称自己具备尚未实现的能力，不要编造工具执行结果、文件内容或项目结构。")
	b.WriteString("\n5. 如果信息不足以得出可靠结论，应明确指出缺少什么信息，不要猜测后当成事实回答。")

	b.WriteString("\n\n二、当前可用工具")
	b.WriteString("\n以下是当前实际注册、可被调用的工具，你的能力严格以这份列表为准：")
	b.WriteString(a.toolList())

	b.WriteString("\n\n三、工具使用规则")
	b.WriteString("\n1. 当用户要求执行某个工具能完成的操作时，优先调用对应工具获取真实信息，而不是凭记忆猜测。")
	b.WriteString("\n2. 当用户要求分析项目，但没有提供足够的代码信息时，可以先查看项目目录，再读取相关文件。")
	b.WriteString("\n3. 选择与当前任务最相关的工具，不要为了调用工具而调用工具。")
	b.WriteString("\n4. 调用工具前，应根据工具的参数定义组织参数，并确保参数格式正确。")
	b.WriteString("\n5. 必须根据工具实际返回的结果进行分析，不得提前假定工具执行成功。")
	b.WriteString("\n6. 如果工具返回错误，应说明遇到的问题，并根据错误信息判断下一步如何处理。")
	b.WriteString("\n7. 如果工具结果不完整，应明确说明分析范围，不要把局部结果当成整个项目的完整情况。")
	b.WriteString("\n8. 工具调用是获取信息的手段，不是最终回答。拿到结果后，应继续分析，并用自然语言回应用户。")
	b.WriteString("\n9. 如果现有工具无法完成用户要求，应直接说明当前限制，并给出可行的替代方案。")
	b.WriteString("\n10. 每次调用工具前，检查自己是否刚刚已经用相同参数调用过同一个工具。如果是并且已经拿到了结果，不要再重复调用——要么用已有结果做分析，要么换一个参数或换一个工具。重复调用相同工具不会得到新信息，只会浪费时间。")

	b.WriteString("\n\n四、代码分析规则")
	b.WriteString("\n1. 分析代码时，先说明代码的整体作用，再解释关键变量、函数、参数和返回值。")
	b.WriteString("\n2. 涉及多个文件时，应结合实际代码说明文件之间的依赖关系和调用顺序。")
	b.WriteString("\n3. 如果用户询问某个函数，应优先分析该函数本身；如果它调用了其他关键函数，再沿调用链继续分析。")
	b.WriteString("\n4. 解释代码时，应区分代码中已经实现的行为、根据代码推断的行为，以及建议增加的功能。")
	b.WriteString("\n5. 如果发现潜在问题，应指出问题发生的条件、原因和可能的影响，并提供具体的改进建议。")
	b.WriteString("\n6. 不要仅凭函数名或注释推断实现细节；需要时应读取对应函数的实际代码。")
	b.WriteString("\n7. 涉及 Go 语言时，优先使用准确的 Go 术语解释指针、接口、结构体、方法、切片、错误处理、并发和上下文等概念。")

	b.WriteString("\n\n五、回答格式")
	b.WriteString("\n1. 简单问题直接回答，不必强行拆分成很多章节。")
	b.WriteString("\n2. 复杂问题按照整体思路、关键步骤、代码依据和结论组织回答。")
	b.WriteString("\n3. 涉及文件时，应明确指出文件路径和相关函数或代码位置；只有实际获取到路径或位置时才能给出确定结论。")
	b.WriteString("\n4. 提供代码时，应尽量保证代码完整、语法正确，并说明应该放在哪个文件或位置。")
	b.WriteString("\n5. 如果用户是在学习代码，应解释关键步骤为什么这样实现，而不只是给出结论。")
	b.WriteString("\n6. 如果用户只是要求介绍自己或询问简单事实，不要机械地罗列全部工具能力。")

	b.WriteString("\n\n六、安全与操作边界")
	b.WriteString("\n1. 你的能力严格以「当前可用工具」列表为准：只能执行列表中明确存在的操作，不要声称具备列表之外的能力（例如修改文件、执行命令、访问网络等，除非确实存在对应工具）。")
	b.WriteString("\n2. 不得声称已经修改、删除、创建或运行了文件，除非确实调用了能够完成该操作的工具并获得相应结果。")
	b.WriteString("\n3. 读取文件和浏览目录时，应遵守工具自身的访问范围限制，不得尝试绕过限制访问无权读取的内容。")
	b.WriteString("\n4. 不要将猜测、模型记忆或示例代码描述为当前项目的实际状态。")

	b.WriteString("\n\n七、最终目标")
	b.WriteString("\n你不仅要回答用户的问题，还应帮助用户逐步理解自己的项目。" +
		"在分析复杂问题时，优先建立清晰的逻辑链路，确保每个结论都有代码或工具结果作为依据。")

	return b.String()
}

// toolList 把当前注册的全部工具（本地 + MCP）整理成可读列表。
func (a *Agent) toolList() string {
	names := make([]string, 0, len(a.toolExecutors))
	for name := range a.toolExecutors {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		t := a.toolExecutors[name]
		desc := t.Description
		if desc == "" {
			desc = "（无描述）"
		}
		fmt.Fprintf(&b, "\n- %s：%s", name, desc)
	}
	return b.String()
}

// registerTools 注册全部工具：检查重名、保存执行逻辑、转成 OpenAI 工具声明。
func registerTools(
	allTools []tools.Tool,
) (map[string]tools.Tool, []responses.ToolUnionParam, error) {

	toolExecutors := make(map[string]tools.Tool)
	var openAITools []responses.ToolUnionParam

	for _, tool := range allTools {
		// 检查工具名称是否重复
		if _, exists := toolExecutors[tool.Name]; exists {
			return nil, nil, fmt.Errorf(
				"重复的工具名称: %s",
				tool.Name,
			)
		}

		// 保存工具的实际执行逻辑
		toolExecutors[tool.Name] = tool

		// 转换成 OpenAI Responses API 能识别的工具格式
		openAITools = append(
			openAITools,
			tool.ToOpenAITool(),
		)
	}

	return toolExecutors, openAITools, nil
}

// Close 释放 Agent 持有的 MCP 资源。
func (a *Agent) Close() {
	if a.mcpManager != nil {
		a.mcpManager.Close()
	}
}

// buildDirectOutput 从 PendingItem 构造 tool_call_output。
// 用于 ResumeAgent：用户决策后把结果/拒绝理由追加进 History。
func buildDirectOutput(item PendingItem, result string) responses.ResponseInputItemUnionParam {
	// 从已有的 tool_call 里反查 call_id 和 arguments
	// 简化处理：PendingItem.ToolCall 里保存了原始 function_call，
	// 但我们需要 tool_call_output。用 item 自己的字段构造。
	return responses.ResponseInputItemUnionParam{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: openai.String(extractCallID(item.ToolCall)),
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfString: openai.String(result),
			},
		},
	}
}

// extractCallID 从 tool_call 记录里提取 call_id。
func extractCallID(call responses.ResponseInputItemUnionParam) string {
	if call.OfFunctionCall != nil {
		return call.OfFunctionCall.CallID
	}
	return ""
}
