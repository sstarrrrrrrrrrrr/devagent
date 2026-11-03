package agent

import (
	"context"
	"devagent/tools"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func runTool(
	ctx context.Context,
	item responses.ResponseOutputItemUnion,
	toolExecutors map[string]tools.Tool,
) (
	responses.ResponseInputItemUnionParam,
	responses.ResponseInputItemUnionParam,
) {
	// 1. 解析模型传来的 JSON 参数
	var args map[string]any

	if err := json.Unmarshal(
		[]byte(item.Arguments.OfString),
		&args,
	); err != nil {
		result := fmt.Sprintf("工具参数解析失败: %v", err)
		return buildToolOutput(item, result)
	}

	// 2. 根据工具名称查找实际工具
	tool, exists := toolExecutors[item.Name]
	if !exists {
		result := fmt.Sprintf("工具不存在: %s", item.Name)
		return buildToolOutput(item, result)
	}

	// 3. 执行工具
	result, err := tool.Execute(ctx, args)
	if err != nil {
		result = fmt.Sprintf("工具执行失败: %v", err)
	}

	// 4. 构造模型需要的工具执行结果
	return buildToolOutput(item, result)
}
func buildToolOutput(
	item responses.ResponseOutputItemUnion,
	result string,
) (
	responses.ResponseInputItemUnionParam,
	responses.ResponseInputItemUnionParam,
) {
	// 工具调用记录。
	toolCall := responses.ResponseInputItemUnionParam{
		OfFunctionCall: &responses.ResponseFunctionToolCallParam{
			Arguments: item.Arguments.OfString,
			CallID:    item.CallID,
			Name:      item.Name,
		},
	}

	// 工具执行结果。
	toolOutput := responses.ResponseInputItemUnionParam{
		OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
			CallID: openai.String(item.CallID),
			Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
				OfString: openai.String(result),
			},
		},
	}

	return toolCall, toolOutput
}
