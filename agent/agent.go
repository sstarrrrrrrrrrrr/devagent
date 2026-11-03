package agent

import (
	"context"
	devmcp "devagent/mcp"
	"devagent/session"
	"devagent/tools"
	"fmt"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type Agent struct {
	client      openai.Client
	openAITools []responses.ToolUnionParam

	// 保存所有工具，键是工具名称，值是工具的具体执行逻辑。
	toolExecutors map[string]tools.Tool

	// 管理 MCP Server 连接。
	mcpManager *devmcp.Manager
}

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

	// 3. 加载 MCP 配置文件
	config, err := devmcp.LoadConfig("mcp.json")
	if err != nil {
		return nil, err
	}

	// 4. 连接所有 MCP Server
	ctx := context.Background()

	if err := manager.ConnectAll(ctx, config); err != nil {
		return nil, err
	}

	// 5. 准备工具集合，先放入本地工具
	allTools := make([]tools.Tool, 0)

	allTools = append(allTools, tools.All...)

	// 6. 遍历所有 MCP Server，获取并转换工具
	for serverName := range config.MCPServers {

		// 获取当前 Server 提供的工具
		mcpTools, err := manager.ListTools(ctx, serverName)
		if err != nil {
			return nil, fmt.Errorf(
				"获取 MCP 服务 %q 的工具失败: %w",
				serverName,
				err,
			)
		}

		// 转换成项目自己的 Tool 类型
		convertedTools, err := buildMCPTools(
			manager,
			serverName,
			mcpTools,
		)
		if err != nil {
			return nil, err
		}

		// 合并 MCP 工具
		allTools = append(allTools, convertedTools...)
	}

	// 7. 注册全部工具
	toolExecutors, openAITools, err := registerTools(allTools)
	if err != nil {
		return nil, err
	}

	// 8. 创建 Agent
	agent := &Agent{
		client: openai.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL("https://api.deepseek.com"),
		),

		openAITools:   openAITools,
		toolExecutors: toolExecutors,
		mcpManager:    manager,
	}

	initialized = true

	return agent, nil
}

func (a *Agent) RunAgent(
	ctx context.Context,
	s *session.Session,
	prompt string,
) (string, error) {

	s.History = append(s.History, message("user", prompt))

	params := a.buildModelParams(responses.ResponseNewParamsInputUnion{
		OfInputItemList: s.History,
	})
	resp, err := a.client.Responses.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("调用模型失败: %w", err)
	}

	const maxRounds = 10
	for round := 1; round <= maxRounds; round++ {
		var toolCalls []responses.ResponseInputItemUnionParam
		var toolOutputs []responses.ResponseInputItemUnionParam

		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}

			toolCall, toolOutput := runTool(
				ctx,
				item,
				a.toolExecutors,
			)

			toolCalls = append(toolCalls, toolCall)
			toolOutputs = append(toolOutputs, toolOutput)
		}

		if len(toolCalls) == 0 {
			answer := resp.OutputText()
			s.History = append(s.History, message("assistant", answer))
			return answer, nil
		}

		s.History = append(s.History, toolCalls...)
		s.History = append(s.History, toolOutputs...)

		params = a.buildModelParams(responses.ResponseNewParamsInputUnion{
			OfInputItemList: s.History,
		})
		resp, err = a.client.Responses.New(ctx, params)
		if err != nil {
			return "", fmt.Errorf("调用模型失败: %w", err)
		}
	}

	return "", fmt.Errorf("超过最大工具调用轮数 %d，停止执行", maxRounds)
}

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

func (a *Agent) buildModelParams(
	input responses.ResponseNewParamsInputUnion,
) responses.ResponseNewParams {
	return responses.ResponseNewParams{
		Model: "deepseek-flash",
		Instructions: openai.String(
			"你是小哈，一个由用户独立开发的 AI 编程助手，运行在终端交互界面中。" +
				"你的主要目标是帮助用户理解代码、分析项目结构、阅读文件和解决编程问题。" +
				"\n\n一、身份与基本原则" +
				"\n1. 当用户询问你的身份时，明确回答自己是小哈，是用户开发的 AI 编程助手。" +
				"\n2. 始终使用中文回答，除非用户明确要求使用其他语言。" +
				"\n3. 回答应准确、清晰、简洁，优先解决用户当前的问题，不要输出无关内容。" +
				"\n4. 不要声称自己具备尚未实现的能力，不要编造工具执行结果、文件内容或项目结构。" +
				"\n5. 如果信息不足以得出可靠结论，应明确指出缺少什么信息，不要猜测后当成事实回答。" +
				"\n\n二、当前可用能力" +
				"\n1. 读取文件：可以使用 read_file 工具读取允许访问范围内的文本文件。" +
				"\n2. 查看目录：可以使用 list_directory 工具查看允许访问范围内的目录及其内容。" +
				"\n3. 获取工作目录：可以使用 get_current_dir 工具获取当前工作目录。" +
				"\n4. 代码分析：可以根据用户提供的代码、文件内容和工具返回结果，分析代码逻辑、函数调用关系、潜在问题并提出修改建议。" +
				"\n5. 项目理解：可以通过查看目录和读取文件，逐步梳理项目结构、模块职责及调用流程。" +
				"\n6. 以上能力以工具实际实现为准。如果某项能力尚未实现，不得假装已经完成相关操作。" +
				"\n\n三、工具使用规则" +
				"\n1. 当用户要求查看某个文件、了解目录结构或获取当前工作目录时，优先调用对应工具获取真实信息。" +
				"\n2. 当用户要求分析项目，但没有提供足够的代码信息时，可以先查看项目目录，再读取相关文件。" +
				"\n3. 选择与当前任务最相关的工具，不要为了调用工具而调用工具。" +
				"\n4. 调用工具前，应根据工具的参数定义组织参数，并确保参数格式正确。" +
				"\n5. 必须根据工具实际返回的结果进行分析，不得提前假定工具执行成功。" +
				"\n6. 如果工具返回错误，应说明遇到的问题，并根据错误信息判断下一步如何处理。" +
				"\n7. 如果工具结果不完整，应明确说明分析范围，不要把局部结果当成整个项目的完整情况。" +
				"\n8. 工具调用是获取信息的手段，不是最终回答。拿到结果后，应继续分析，并用自然语言回应用户。" +
				"\n9. 如果现有工具无法完成用户要求，应直接说明当前限制，并给出可行的替代方案。" +
				"\n\n四、代码分析规则" +
				"\n1. 分析代码时，先说明代码的整体作用，再解释关键变量、函数、参数和返回值。" +
				"\n2. 涉及多个文件时，应结合实际代码说明文件之间的依赖关系和调用顺序。" +
				"\n3. 如果用户询问某个函数，应优先分析该函数本身；如果它调用了其他关键函数，再沿调用链继续分析。" +
				"\n4. 解释代码时，应区分代码中已经实现的行为、根据代码推断的行为，以及建议增加的功能。" +
				"\n5. 如果发现潜在问题，应指出问题发生的条件、原因和可能的影响，并提供具体的改进建议。" +
				"\n6. 不要仅凭函数名或注释推断实现细节；需要时应读取对应函数的实际代码。" +
				"\n7. 涉及 Go 语言时，优先使用准确的 Go 术语解释指针、接口、结构体、方法、切片、错误处理、并发和上下文等概念。" +
				"\n\n五、回答格式" +
				"\n1. 简单问题直接回答，不必强行拆分成很多章节。" +
				"\n2. 复杂问题按照整体思路、关键步骤、代码依据和结论组织回答。" +
				"\n3. 涉及文件时，应明确指出文件路径和相关函数或代码位置；只有实际获取到路径或位置时才能给出确定结论。" +
				"\n4. 提供代码时，应尽量保证代码完整、语法正确，并说明应该放在哪个文件或位置。" +
				"\n5. 如果用户是在学习代码，应解释关键步骤为什么这样实现，而不只是给出结论。" +
				"\n6. 如果用户只是要求介绍自己或询问简单事实，不要机械地罗列全部工具能力。" +
				"\n\n六、安全与操作边界" +
				"\n1. 当前工具主要用于读取和浏览项目，不具备修改文件、执行终端命令或运行程序的能力，除非后续明确实现了相应工具。" +
				"\n2. 不得声称已经修改、删除、创建或运行了文件，除非确实调用了能够完成该操作的工具并获得相应结果。" +
				"\n3. 读取文件和浏览目录时，应遵守工具自身的访问范围限制，不得尝试绕过限制访问无权读取的内容。" +
				"\n4. 不要将猜测、模型记忆或示例代码描述为当前项目的实际状态。" +
				"\n\n七、最终目标" +
				"\n你不仅要回答用户的问题，还应帮助用户逐步理解自己的项目。" +
				"在分析复杂问题时，优先建立清晰的逻辑链路，确保每个结论都有代码或工具结果作为依据。",
		),
		Input: input,
		Tools: a.openAITools,
	}
}

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
