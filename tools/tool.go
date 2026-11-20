package tools

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// RiskLevel 定义工具操作的风险等级，SafetyGuard 据此决定拦截策略。
type RiskLevel int

const (
	// L0Safe 只读、无副作用，直接放行。
	L0Safe RiskLevel = iota
	// L1Confirm 有轻微副作用（创建/修改），默认需要用户确认。
	// 安全策略匹配白名单或参数级判断后可升级或降级。
	L1Confirm
	// L2Review 有较大副作用（覆盖/删除），需要暂停 Agent 并让用户审核。
	L2Review
	// L3Blocked 无论什么情况都禁止，直接拒绝。
	L3Blocked
)

// String 返回风险等级的可读名称。
func (r RiskLevel) String() string {
	switch r {
	case L0Safe:
		return "L0-Safe"
	case L1Confirm:
		return "L1-Confirm"
	case L2Review:
		return "L2-Review"
	case L3Blocked:
		return "L3-Blocked"
	default:
		return "L2-Confirm"
	}
}

// Tool 描述一个可供模型调用的工具。
type Tool struct {
	Name        string
	Description string
	Execute     func(ctx context.Context, args map[string]any) (string, error)
	Parameters  map[string]any

	// RiskLevel 工具的基础风险等级。
	// SafetyGuard.Decide() 会根据策略/参数动态调整，此值为初始值。
	RiskLevel RiskLevel
}


// ToOpenAITool 把工具转成 OpenAI 的工具声明。
func (t Tool) ToOpenAITool() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfFunction: &responses.FunctionToolParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  t.Parameters,
		},
	}
}

// All 是 Agent 使用的本地工具集合，新增本地工具时在这里登记。
// 除 run_command 外都是只读的 L0Safe；run_command 是执行类 L2Review，
// 由 SafetyGuard 强制走用户审核。
var All = []Tool{
	ReadFileTool,
	CurrentDirTool,
	ListFilesTool,
	RunCommandTool,
}
