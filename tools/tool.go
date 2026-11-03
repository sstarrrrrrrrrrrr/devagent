package tools

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// Tool 描述一个可供模型调用的工具。
type Tool struct {
	Name        string
	Description string
	Execute     func(ctx context.Context, args map[string]any) (string, error)
	Parameters  map[string]any
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
var All = []Tool{ReadFileTool, CurrentDirTool, ListFilesTool}
