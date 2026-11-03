package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ListFilesTool 列出指定目录下的文件和子目录。
var ListFilesTool = Tool{
	Name:        "list_files",
	Description: "列出指定目录下的文件和子目录",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要查看的目录路径，可以是项目内的相对路径或绝对路径",
			},
		},
		"additionalProperties": false,
	},
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		path := "."
		if value, exists := args["path"]; exists {
			pathValue, ok := value.(string)
			if !ok || strings.TrimSpace(pathValue) == "" {
				return "", fmt.Errorf("path 参数必须是非空字符串")
			}
			path = pathValue
		}

		root, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("获取当前根目录失败：%w", err)
		}

		realTarget, err := resolvePathWithin(root, path)
		if err != nil {
			return "", err
		}

		info, err := os.Stat(realTarget)
		if err != nil {
			return "", fmt.Errorf("获取目标路径信息失败: %w", err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("目标路径不是目录")
		}

		entries, err := os.ReadDir(realTarget)
		if err != nil {
			return "", fmt.Errorf("读取目录失败: %w", err)
		}
		if len(entries) > 200 {
			entries = entries[:200]
		}

		var result strings.Builder
		for _, entry := range entries {
			if entry.IsDir() {
				fmt.Fprintf(&result, "[目录] %s/\n", entry.Name())
			} else {
				fmt.Fprintf(&result, "[文件] %s\n", entry.Name())
			}
		}
		if result.Len() == 0 {
			return "该目录为空。", nil
		}
		return result.String(), nil
	},
}
