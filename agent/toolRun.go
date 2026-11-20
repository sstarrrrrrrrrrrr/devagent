package agent

import (
	"context"
	"devagent/safety"
	"devagent/tools"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// RunResult 是 runTool 的完整返回值，有三种可能：
//   - Confirmed=true, Approved=true  → 已执行完（用户允许过，或工具本身是 Allow）
//   - Confirmed=false                 → 需要用户确认，暂停 Agent
//   - Approved=false, Confirmed=true  → 用户拒绝了（ResumeAgent 传 false 后会走这个分支）
type RunResult struct {
	// ToolCall 是模型 function_call 的原始记录（已追加进 History）。
	ToolCall responses.ResponseInputItemUnionParam
	// ToolOutput 是工具执行结果（已追加进 History）。
	// 当 ToolName/Decision 非空时，这个 Output 还没生成（需要用户确认后才执行）。
	ToolOutput responses.ResponseInputItemUnionParam

	// 需要用户确认时填充以下字段：
	ToolName string
	ArgsMap  map[string]any
	Decision safety.Decision
	ToolDesc string
}

// runTool 执行模型请求的工具。
// 安全拦截器可能返回三种情况：
//  1. ActionAllow → 直接执行
//  2. ActionBlock → 返回错误（工具 Output 是"此操作被禁止"）
//  3. ActionRequireConfirm → 返回 ErrRequireConfirm，RunResult 里带完整信息让上层处理
func runTool(
	ctx context.Context,
	item responses.ResponseOutputItemUnion,
	toolExecutors map[string]tools.Tool,
	safetyGuard *safety.SafetyGuard,
) RunResult {
	// 1. 解析模型传来的 JSON 参数
	var args map[string]any
	if err := json.Unmarshal([]byte(item.Arguments.OfString), &args); err != nil {
		return RunResult{
			ToolCall:  buildToolCall(item),
			ToolOutput: buildToolOutput(item, fmt.Sprintf("工具参数解析失败: %v", err)),
		}
	}

	// 2. 根据工具名称查找实际工具
	tool, exists := toolExecutors[item.Name]
	if !exists {
		return RunResult{
			ToolCall:  buildToolCall(item),
			ToolOutput: buildToolOutput(item, fmt.Sprintf("工具不存在: %s", item.Name)),
		}
	}

	// 3. 先用 SafetyGuard 做一次预判断（不执行），知道是否需要用户确认
	if safetyGuard != nil {
		decision := safetyGuard.Decide(tool, args)

		if decision.Action == safety.ActionRequireConfirm {
			// 暂停，等用户决策
			return RunResult{
				ToolCall:   buildToolCall(item),
				ToolOutput: buildToolOutput(item, ""), // 占位，还没执行
				ToolName:   item.Name,
				ArgsMap:    args,
				Decision:   decision,
				ToolDesc:   tool.Description,
			}
		}
	}

	// 4. 执行工具（Allow 或 Block 都走到这里：
	//    - Allow → 正常执行
	//    - Block → SafetyGuard 包装的 Execute 会返回明确错误）
	result, err := tool.Execute(ctx, args)
	if err != nil {
		// 如果是我们的 ErrRequireConfirm → 理论上不会走到这里（上面已经 return 了），
		// 但保险起见处理一下
		if errors.Is(err, safety.ErrRequireConfirm) {
			decision := safetyGuard.Decide(tool, args)
			return RunResult{
				ToolCall:   buildToolCall(item),
				ToolOutput: buildToolOutput(item, ""),
				ToolName:   item.Name,
				ArgsMap:    args,
				Decision:   decision,
				ToolDesc:   tool.Description,
			}
		}
		result = fmt.Sprintf("工具执行失败: %v", err)
	}

	return RunResult{
		ToolCall:  buildToolCall(item),
		ToolOutput: buildToolOutput(item, result),
	}
}

// buildToolCall 构造模型的 function_call 记录（不做任何执行）。
func buildToolCall(item responses.ResponseOutputItemUnion) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{
		OfFunctionCall: &responses.ResponseFunctionToolCallParam{
			Arguments: item.Arguments.OfString,
			CallID:    item.CallID,
			Name:      item.Name,
		},
	}
}

// buildToolOutput 构造模型的 function_call_output 执行结果。
func buildToolOutput(item responses.ResponseOutputItemUnion, result string) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: openai.String(item.CallID),
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfString: openai.String(result),
			},
		},
	}
}
