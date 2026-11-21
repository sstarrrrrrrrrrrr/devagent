package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// maxCommandOutput 是 run_command 单次返回给模型的最大字节数，防止海量输出撑爆上下文。
const maxCommandOutput = 64 * 1024

// 命令超时上下限：默认 2 分钟，最长 10 分钟。
const (
	defaultCommandTimeout = 120 * time.Second
	maxCommandTimeout     = 10 * time.Minute
)

// RunCommandTool 在项目根目录执行 shell 命令并返回输出。
// 用于 git 操作、构建、测试等需要真实命令行的场景。
// 风险等级 L2Review：SafetyGuard 按执行类语义自动要求用户审核，
// Blocklist.CommandPatterns 也能直接拦截危险命令（工具侧无需重复实现）。
var RunCommandTool = Tool{
	Name:        "run_command",
	Description: "在项目根目录执行一条 shell 命令（Windows 用 cmd /c，其他平台用 sh -c），返回合并后的 stdout/stderr。支持 git、构建、测试等命令。执行前需要用户审核确认",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "要执行的完整命令行，如 \"git status\" 或 \"go build ./...\"",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "超时秒数，默认 120，最大 600",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	},
	// 执行类基础等级 L2，配合 classifyBySemantics 的 toolKindExec 结果保持一致。
	// 即使用户配置了 RiskOverrides 降级，黑名单 pattern 仍然兜底。
	RiskLevel:    L2Review,
	ExplicitRisk: true,
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		commandValue, ok := args["command"].(string)
		if !ok || strings.TrimSpace(commandValue) == "" {
			return "", fmt.Errorf("command 参数必须是非空字符串")
		}

		// 解析超时：缺省 120s，上限 600s
		timeout := defaultCommandTimeout
		if t, ok := args["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t) * time.Second
			if timeout > maxCommandTimeout {
				timeout = maxCommandTimeout
			}
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// 通过系统 shell 执行，支持管道、&&、重定向等完整语法
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "windows":
			cmd = exec.CommandContext(ctx, "cmd", "/c", commandValue)
		default:
			cmd = exec.CommandContext(ctx, "sh", "-c", commandValue)
		}
		cmd.Dir = workdirRoot()

		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf

		if err := ctx.Err(); err != nil {
			return "", err
		}

		runErr := cmd.Run()

		output := buf.String()
		// 超长输出截断，保留开头（报错信息通常在前面）
		if len(output) > maxCommandOutput {
			output = output[:maxCommandOutput] + "\n...（输出过大，已截断）"
		}

		// 命令本身的非零退出码不算工具失败：输出（含 stderr）已经足够
		// 让模型判断发生了什么，原样返回即可。
		if runErr != nil {
			if ctx.Err() != nil {
				return output, fmt.Errorf("命令超时（超过 %s）被终止", timeout)
			}
			exitCode := -1
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			return fmt.Sprintf("%s\n（命令退出码 %d）", output, exitCode), nil
		}

		return output, nil
	},
}
