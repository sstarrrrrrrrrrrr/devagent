package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxReadSize = 64 * 1024

var ReadFileTool = Tool{
	Name:        "read_file",
	Description: "读取项目根目录内的文本文件，不允许访问项目目录之外的文件",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "需要读取的文件路径，可以是项目内的相对路径或绝对路径",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	},
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		pathValue, ok := args["path"].(string)
		if !ok || strings.TrimSpace(pathValue) == "" {
			return "", fmt.Errorf("path 参数必须是非空字符串")
		}

		root, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("获取当前工作目录失败: %w", err)
		}

		realTarget, err := resolvePathWithin(root, pathValue)
		if err != nil {
			return "", err
		}

		// 确认目标是普通文件，而不是目录。
		info, err := os.Stat(realTarget)
		if err != nil {
			return "", fmt.Errorf("获取文件信息失败: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("只允许读取普通文件")
		}

		if err := ctx.Err(); err != nil {
			return "", err
		}

		f, err := os.Open(realTarget)
		if err != nil {
			return "", fmt.Errorf("打开文件失败: %w", err)
		}
		defer f.Close()

		// 只读取上限内的内容，避免一次性把超大文件读进内存。
		data, err := io.ReadAll(io.LimitReader(f, maxReadSize+1))
		if err != nil {
			return "", fmt.Errorf("读取文件失败: %w", err)
		}

		if int64(len(data)) > maxReadSize {
			return string(data[:maxReadSize]) + "\n...（文件过大，已截断）", nil
		}
		return string(data), nil
	},
}
