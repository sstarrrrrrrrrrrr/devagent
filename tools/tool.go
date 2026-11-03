package tools

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

type Tool struct {
	Name        string
	Description string
	Execute     func(ctx context.Context, args map[string]any) (string, error)
	Parameters  map[string]any
}

func (t Tool) ToOpenAITool() responses.ToolUnionParam {
	return responses.ToolUnionParam{
		OfFunction: &responses.FunctionToolParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  t.Parameters,
		},
	}
}

var All = []Tool{ReadFileTool, CurrentDirTool, ListFilesTool}

func Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	for _, t := range All {
		if t.Name == name {
			return t.Execute(ctx, args)
		}
	}
	return "", fmt.Errorf("tool %s not found", name)
}
