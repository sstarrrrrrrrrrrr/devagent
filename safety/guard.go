package safety

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	projecttools "devagent/tools"
)

// ErrRequireConfirm 表示工具需要用户确认。
// wrapWithSafetyGuard 包装的 Execute 在 ActionRequireConfirm 时返回此错误，
// Agent 的 runTool 捕获后暂停执行，把工具信息交给 TUI 弹窗。
var ErrRequireConfirm = errors.New("安全策略要求用户确认后才能执行")

// PendingConfirm 是需要人机协作时传递的完整信息。
// Agent 把它交给 TUI，TUI 弹框展示后等用户决策。
type PendingConfirm struct {
	ToolName  string
	Arguments map[string]any
	Decision  Decision
	ToolDesc  string // 工具描述（展示给用户看）
}

// Action 是 SafetyGuard.Decide() 对一次工具调用的决策。
// 只有三种，语义绝对清晰：
type Action int

const (
	// ActionAllow 直接放行，不用问人，Execute 正常执行。
	ActionAllow Action = iota
	// ActionRequireConfirm 暂停，等用户决策后再执行。
	// Execute 返回 ErrRequireConfirm，Agent 把工具调用信息交给 TUI。
	ActionRequireConfirm
	// ActionBlock 直接拒绝，永远不执行。Execute 返回明确错误。
	ActionBlock
)

// Decision 是对一次工具调用的完整决策结果。
type Decision struct {
	// Action 决策动作
	Action Action
	// Reason 决策原因（展示给用户看）
	Reason string
	// RiskLevel 最终生效的风险等级
	RiskLevel projecttools.RiskLevel
}

// SafetyGuard 是安全策略的运行时执行器。
// 在工具注册时用 wrapWithSafetyGuard 把它包装进每个 Tool 的 Execute 闭包里。
type SafetyGuard struct {
	config *SafetyConfig

	// 预编译的 regex pattern（加速命令匹配）
	compiledCommands []*regexp.Regexp

	// 工作目录根路径（路径沙箱参考点）
	workdir string
}

// NewSafetyGuard 创建安全拦截器。
func NewSafetyGuard(config *SafetyConfig, workdir string) (*SafetyGuard, error) {
	if config == nil {
		return nil, fmt.Errorf("SafetyConfig 不能为 nil")
	}

	// 预编译所有 regex pattern
	var compiled []*regexp.Regexp
	for _, pattern := range config.Blocklist.CommandPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("编译命令 pattern %q 失败: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}

	wd := workdir
	if wd == "" && config.Workdir != "" {
		wd = config.Workdir
	}

	return &SafetyGuard{
		config:           config,
		compiledCommands: compiled,
		workdir:          wd,
	}, nil
}

// Decide 对一次工具调用做完整的拦截器链判定。
//
// 优先级从高到低（上一层命中 → 直接返回，不看下一层）：
//  1. Blocklist.Tools 命中 → Blocked
//  2. Blocklist.Paths 命中 → Blocked
//  3. Blocklist.CommandPatterns 命中 → Blocked
//  4. Whitelist.Tools 命中 → Allow（跳过所有后续检查）
//  5. Whitelist.Paths 命中 → Allow
//  6. RiskOverrides 覆盖 tool.RiskLevel → 升级或降级
//  7. tool.RiskLevel 本身 → 映射到 Allow / RequireConfirm
//  8. 兜底 DefaultRisk
func (g *SafetyGuard) Decide(tool projecttools.Tool, args map[string]any) Decision {
	// ---- 第 1 层：Blocklist → 直接 Block ----

	if matchAnyGlob(tool.Name, g.config.Blocklist.Tools) {
		return Decision{
			Action:    ActionBlock,
			Reason:    fmt.Sprintf("工具名 %q 匹配黑名单", tool.Name),
			RiskLevel: projecttools.L3Blocked,
		}
	}

	if matchedArg, matchedPattern := g.matchBlockedPath(args); matchedPattern != "" {
		return Decision{
			Action:    ActionBlock,
			Reason:    fmt.Sprintf("参数 %q 的路径 %q 匹配黑名单 %q", matchedArg, args[matchedArg], matchedPattern),
			RiskLevel: projecttools.L3Blocked,
		}
	}

	if matchedArg, matchedPattern := g.matchBlockedCommand(args); matchedPattern != "" {
		return Decision{
			Action:    ActionBlock,
			Reason:    fmt.Sprintf("参数 %q 的命令 %q 匹配黑名单 pattern", matchedArg, args[matchedArg]),
			RiskLevel: projecttools.L3Blocked,
		}
	}

	// ---- 第 2 层：Whitelist → 直接 Allow ----

	if matchAnyGlob(tool.Name, g.config.Whitelist.Tools) {
		return Decision{
			Action:    ActionAllow,
			Reason:    fmt.Sprintf("工具 %q 在白名单内", tool.Name),
			RiskLevel: projecttools.L0Safe,
		}
	}

	if matchedArg, matchedPattern := g.matchWhitelistedPath(args); matchedPattern != "" {
		_ = matchedArg
		return Decision{
			Action:    ActionAllow,
			Reason:    fmt.Sprintf("路径匹配白名单 %q", matchedPattern),
			RiskLevel: projecttools.L0Safe,
		}
	}

	// ---- 第 3 层：RiskOverrides 覆盖 ----
	effectiveLevel := tool.RiskLevel
	for pattern, riskStr := range g.config.RiskOverrides {
		if matchGlob(tool.Name, pattern) {
			if level, ok := parseRiskLevel(riskStr); ok {
				effectiveLevel = level
			}
		}
	}

	// ---- 第 4 层：兜底 RiskLevel ----
	if effectiveLevel == 0 {
		if g.config.DefaultRisk != "" {
			if level, ok := parseRiskLevel(g.config.DefaultRisk); ok {
				effectiveLevel = level
			} else {
				effectiveLevel = projecttools.L1Confirm
			}
		} else {
			effectiveLevel = projecttools.L1Confirm
		}
	}

	// ---- 第 5 层：RiskLevel → Action（三态映射） ----
	switch effectiveLevel {
	case projecttools.L0Safe:
		return Decision{Action: ActionAllow, Reason: "L0-Safe，直接放行", RiskLevel: effectiveLevel}
	case projecttools.L1Confirm:
		return Decision{Action: ActionRequireConfirm, Reason: "L1-Confirm，需要用户确认", RiskLevel: effectiveLevel}
	case projecttools.L2Review:
		return Decision{Action: ActionRequireConfirm, Reason: "L2-Review，需要用户审核", RiskLevel: effectiveLevel}
	case projecttools.L3Blocked:
		return Decision{Action: ActionBlock, Reason: "L3-Blocked，禁止执行", RiskLevel: effectiveLevel}
	default:
		return Decision{Action: ActionRequireConfirm, Reason: "未知等级，默认需要确认", RiskLevel: projecttools.L1Confirm}
	}
}

// ---- 匹配辅助方法 ----

// matchBlockedPath 扫描 args 里所有 string 类型参数，看是否匹配 Blocklist.Paths。
func (g *SafetyGuard) matchBlockedPath(args map[string]any) (string, string) {
	for _, pattern := range g.config.Blocklist.Paths {
		for argName, argValue := range args {
			pathStr, ok := argValue.(string)
			if !ok {
				continue
			}
			if matchPathGlob(pathStr, pattern) {
				return argName, pattern
			}
		}
	}
	return "", ""
}

// matchWhitelistedPath 扫描 args 里所有 string 类型参数，看是否匹配 Whitelist.Paths。
func (g *SafetyGuard) matchWhitelistedPath(args map[string]any) (string, string) {
	for _, pattern := range g.config.Whitelist.Paths {
		for argName, argValue := range args {
			pathStr, ok := argValue.(string)
			if !ok {
				continue
			}
			if matchPathGlob(pathStr, pattern) {
				return argName, pattern
			}
		}
	}
	return "", ""
}

// matchBlockedCommand 扫描 args 里所有 string 类型参数，看是否匹配 Blocklist.CommandPatterns。
func (g *SafetyGuard) matchBlockedCommand(args map[string]any) (string, string) {
	for _, re := range g.compiledCommands {
		for argName, argValue := range args {
			cmdStr, ok := argValue.(string)
			if !ok {
				continue
			}
			if re.MatchString(cmdStr) {
				return argName, re.String()
			}
		}
	}
	return "", ""
}

// matchAnyGlob 测试 name 是否匹配 patterns 里的任意一个 glob。
func matchAnyGlob(name string, patterns []string) bool {
	for _, p := range patterns {
		if matchGlob(name, p) {
			return true
		}
	}
	return false
}

// matchGlob 测试 name 是否匹配 pattern（支持 '*' 和 '?'）。
func matchGlob(name, pattern string) bool {
	ok, _ := filepath.Match(pattern, name)
	return ok
}

// matchPathGlob 测试 path 是否匹配 pattern。
// 先做平台归一化（Windows \ → /），再做 filepath.Match。
func matchPathGlob(path, pattern string) bool {
	path = strings.ReplaceAll(path, "\\", "/")
	pattern = strings.ReplaceAll(pattern, "\\", "/")

	if strings.HasPrefix(path, "~") {
		path = path[1:]
	}
	if strings.HasPrefix(pattern, "~") {
		pattern = pattern[1:]
	}

	ok, err := filepath.Match(pattern, path)
	if err == nil && ok {
		return true
	}

	return strings.Contains(path, strings.TrimSuffix(strings.TrimPrefix(pattern, "*/"), "/*"))
}

// parseRiskLevel 解析 risk level 字符串（大小写不敏感）。
func parseRiskLevel(s string) (projecttools.RiskLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "l0", "l0safe", "safe":
		return projecttools.L0Safe, true
	case "l1", "l1confirm", "confirm":
		return projecttools.L1Confirm, true
	case "l2", "l2review", "review":
		return projecttools.L2Review, true
	case "l3", "l3blocked", "blocked", "deny":
		return projecttools.L3Blocked, true
	default:
		return projecttools.L0Safe, false
	}
}

// ---- 装饰器：包装 Tool ----

// wrapWithSafetyGuard 用安全拦截器包装原始 Tool.Execute。
//
// 返回的新 Tool.Execute 行为：
//  - ActionAllow → 直接调用原始 Execute
//  - ActionBlock → 返回 error "此操作被安全策略禁止"
//  - ActionRequireConfirm → 返回 error ErrRequireConfirm
//
// Agent 的 runTool 捕获到 ErrRequireConfirm 后，会调用 SafetyGuard.Decide()
// 重新拿到完整 Decision（Decision 是纯计算，无副作用，重调一次无成本）。
func wrapWithSafetyGuard(original projecttools.Tool, guard *SafetyGuard) projecttools.Tool {
	if guard == nil {
		return original
	}

	tool := original
	origExecute := tool.Execute

	tool.Execute = func(ctx context.Context, args map[string]any) (string, error) {
		decision := guard.Decide(original, args)

		switch decision.Action {
		case ActionBlock:
			return "", fmt.Errorf(
				"⚠️  此操作被安全策略禁止\n"+
					"工具: %s\n"+
					"原因: %s\n"+
					"风险等级: %s",
				original.Name, decision.Reason, decision.RiskLevel.String(),
			)

		case ActionRequireConfirm:
			// 返回特殊错误，Agent 捕获后暂停，TUI 弹窗等用户决策
			return "", fmt.Errorf("%w: %s (工具=%s, 风险=%s)",
				ErrRequireConfirm, decision.Reason, original.Name, decision.RiskLevel.String())

		case ActionAllow:
			fallthrough
		default:
			return origExecute(ctx, args)
		}
	}

	return tool
}

// WrapAllTools 批量包装工具列表。
func WrapAllTools(tools []projecttools.Tool, guard *SafetyGuard) []projecttools.Tool {
	if guard == nil {
		return tools
	}
	result := make([]projecttools.Tool, len(tools))
	for i, tool := range tools {
		result[i] = wrapWithSafetyGuard(tool, guard)
	}
	return result
}

// ---- context 辅助 ----

const maxArgStringLen = 200

func truncateForLog(s string) string {
	if len(s) <= maxArgStringLen {
		return s
	}
	return s[:maxArgStringLen] + fmt.Sprintf("...(截断，总长度 %d)", len(s))
}
