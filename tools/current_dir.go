package tools

import (
	"context"
	"os"
)

// CurrentDirTool 获取当前程序所在的工作目录。
var CurrentDirTool = Tool{
	Name:        "get_current_dir",
	Description: "获取当前程序所在的工作目录",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{},
		"required":   []string{},
	},
	// 纯元信息 → L0Safe
	RiskLevel:    L0Safe,
	ExplicitRisk: true,
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return dir, nil
	},
}
