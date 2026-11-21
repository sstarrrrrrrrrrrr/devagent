package safety

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	projecttools "devagent/tools"
)

// ErrRequireConfirm 表示工具需要用户确认。
// wrapWithSafetyGuard 包装的 Execute 在 ActionRequireConfirm 时返回此错误，
// Agent 的 runTool 捕获后暂停执行，把工具信息交给 TUI 弹窗。
var ErrRequireConfirm = errors.New("安全策略要求用户确认后才能执行")

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
//  6. RiskOverrides 命中 → 使用用户配置的等级
//  7. 按工具语义自动分类：只读（目录内）→ L0；目录外读取 → L1；
//     写入 / 删除 / 执行 → L2；无法识别 → L1
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

	// ---- 第 3 层：RiskOverrides 覆盖（用户显式配置优先） ----
	effectiveLevel := tool.RiskLevel
	reason := ""
	overridden := false
	for pattern, riskStr := range g.config.RiskOverrides {
		if matchGlob(tool.Name, pattern) {
			if level, ok := parseRiskLevel(riskStr); ok {
				effectiveLevel = level
				overridden = true
				reason = fmt.Sprintf("匹配 riskOverrides 规则 %q", pattern)
			}
		}
	}

	// ---- 第 4 层：按工具语义自动分类 ----
	// 没有显式覆盖时，按「读取 / 写入 / 删除 / 执行」判断风险，
	// 避免把只读工具也当成需要确认的操作。
	if !overridden {
		effectiveLevel, reason = g.autoClassify(tool, args)
	}

	// ---- 第 5 层：RiskLevel → Action（三态映射） ----
	// 注意：L0Safe 的数值就是 0，不能用 effectiveLevel == 0 判断
	// 「等级未设置」，否则会把只读工具的 L0 误判成兜底等级。
	if reason == "" {
		reason = defaultReason(effectiveLevel)
	}

	switch effectiveLevel {
	case projecttools.L0Safe:
		return Decision{Action: ActionAllow, Reason: reason, RiskLevel: effectiveLevel}
	case projecttools.L1Confirm:
		return Decision{Action: ActionRequireConfirm, Reason: reason, RiskLevel: effectiveLevel}
	case projecttools.L2Review:
		return Decision{Action: ActionRequireConfirm, Reason: reason, RiskLevel: effectiveLevel}
	case projecttools.L3Blocked:
		return Decision{Action: ActionBlock, Reason: reason, RiskLevel: effectiveLevel}
	default:
		return Decision{Action: ActionRequireConfirm, Reason: "未知等级，默认需要确认", RiskLevel: projecttools.L1Confirm}
	}
}

// defaultReason 返回风险等级的默认说明。
func defaultReason(level projecttools.RiskLevel) string {
	switch level {
	case projecttools.L0Safe:
		return "L0-Safe，直接放行"
	case projecttools.L1Confirm:
		return "L1-Confirm，需要用户确认"
	case projecttools.L2Review:
		return "L2-Review，需要用户审核"
	case projecttools.L3Blocked:
		return "L3-Blocked，禁止执行"
	default:
		return "未知风险等级"
	}
}

// toolKind 是工具语义分类。
type toolKind int

const (
	toolKindUnknown toolKind = iota
	toolKindRead
	toolKindWrite
	toolKindDestroy
	toolKindExec
)

// 工具名关键词表。判断顺序：删除 > 执行 > 写入 > 读取。
// 名称匹配不区分大小写，MCP 工具的完整名形如 mcp_<server>_<tool>，直接子串匹配即可。
var (
	destroyKeywords = []string{
		"delete", "remove", "unlink", "truncate", "drop", "destroy",
		"wipe", "purge", "rmdir", "kill", "clear",
	}
	execKeywords = []string{
		"exec", "shell", "bash", "powershell", "terminal", "spawn",
		"command", "cmd", "process", "run",
	}
	writeKeywords = []string{
		"write", "create", "edit", "replace", "append", "patch", "update",
		"mkdir", "move", "rename", "copy", "chmod", "chown", "symlink",
		"upload", "push", "deploy", "publish", "commit", "merge",
	}
	readKeywords = []string{
		"read", "get", "list", "search", "find", "stat", "info", "view",
		"cat", "tree", "glob", "grep", "diff", "describe", "query",
		"fetch", "download", "open", "head", "tail", "exists", "count",
	}
)

// classifyTool 根据工具名推断语义类别。
func classifyTool(name string) toolKind {
	n := strings.ToLower(name)

	for _, k := range destroyKeywords {
		if strings.Contains(n, k) {
			return toolKindDestroy
		}
	}
	for _, k := range execKeywords {
		if strings.Contains(n, k) {
			return toolKindExec
		}
	}
	for _, k := range writeKeywords {
		if strings.Contains(n, k) {
			return toolKindWrite
		}
	}
	for _, k := range readKeywords {
		if strings.Contains(n, k) {
			return toolKindRead
		}
	}
	return toolKindUnknown
}

// autoClassify 决定一次调用的基础风险等级。
//
// 本地工具都显式声明了 RiskLevel（ExplicitRisk=true），以声明为准
// （L0 放行 / L1、L2 确认 / L3 拒绝）；MCP 工具的 RiskLevel 只是占位值，
// 按语义 + 参数路径推断：
//   - 只读工具访问工作目录内 → L0 直接放行
//   - 只读工具访问工作目录外 → L1 需要确认
//   - 写入 / 删除 / 命令执行 → L2 需要审核
//   - 无法识别的工具 → DefaultRisk（默认 L1）
func (g *SafetyGuard) autoClassify(tool projecttools.Tool, args map[string]any) (projecttools.RiskLevel, string) {
	if tool.ExplicitRisk {
		return tool.RiskLevel, fmt.Sprintf("工具声明的风险等级为 %s，按该等级处理", tool.RiskLevel)
	}
	return g.classifyBySemantics(tool, args)
}

// classifyBySemantics 只按工具名语义和参数路径推断等级。
func (g *SafetyGuard) classifyBySemantics(tool projecttools.Tool, args map[string]any) (projecttools.RiskLevel, string) {
	switch classifyTool(tool.Name) {
	case toolKindRead:
		if outside := g.pathsOutsideWorkdir(args); len(outside) > 0 {
			return projecttools.L1Confirm, fmt.Sprintf(
				"只读工具要访问工作目录之外的文件 %q，需要确认", outside[0])
		}
		return projecttools.L0Safe, "只读工具且路径都在工作目录内，直接放行"

	case toolKindWrite:
		return projecttools.L2Review, "写操作（新增/修改文件），需要审核"

	case toolKindDestroy:
		return projecttools.L2Review, "删除或破坏性操作，需要审核"

	case toolKindExec:
		return projecttools.L2Review, "命令执行，需要审核"

	default:
		if g.config.DefaultRisk != "" {
			if level, ok := parseRiskLevel(g.config.DefaultRisk); ok {
				return level, "未识别的工具，使用配置的默认等级"
			}
		}
		return projecttools.L1Confirm, "工具用途未知，需要确认"
	}
}

// isPathArg 判断参数名是否表示路径。
func isPathArg(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{
		"path", "file", "dir", "folder", "target", "source", "root", "location",
	} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// pathsOutsideWorkdir 返回参数里落在工作目录之外的路径。
func (g *SafetyGuard) pathsOutsideWorkdir(args map[string]any) []string {
	if g.workdir == "" {
		return nil
	}
	wd, err := filepath.Abs(g.workdir)
	if err != nil {
		return nil
	}

	var outside []string
	for name, value := range args {
		if !isPathArg(name) {
			continue
		}
		s, ok := value.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}

		p := s
		if !filepath.IsAbs(p) {
			p = filepath.Join(wd, p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			outside = append(outside, s)
			continue
		}

		rel, err := filepath.Rel(wd, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			outside = append(outside, s)
		}
	}
	return outside
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
//
// 处理要点：
//   - Windows 反斜杠归一化为 /
//   - ~ 展开为用户主目录
//   - 大小写不敏感（Windows 文件系统语义）
//   - 目录模式 "dir/*" 命中 dir 本身及其下任意层级
//   - 不做「子串包含」匹配，否则 "/dev/*" 会误伤 "D:/code/.../devagent"
func matchPathGlob(path, pattern string) bool {
	path = strings.ToLower(normalizeMatchPath(path))
	pattern = strings.ToLower(normalizeMatchPath(pattern))

	if ok, err := filepath.Match(pattern, path); err == nil && ok {
		return true
	}

	if strings.HasSuffix(pattern, "/*") {
		dir := strings.TrimSuffix(pattern, "/*")
		if path == dir || strings.HasPrefix(path, dir+"/") {
			return true
		}
	}

	return false
}

// normalizeMatchPath 统一路径分隔符并展开 ~。
func normalizeMatchPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.ReplaceAll(p, "//", "/")

	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			home = strings.ReplaceAll(home, "\\", "/")
			home = strings.ReplaceAll(home, "//", "/")
			p = strings.TrimSuffix(home, "/") + p[1:]
		}
	}

	return p
}

// parseRiskLevel 解析 risk level 字符串。
// 大小写、连字符、下划线、空格均不敏感：
// "L3-Blocked" / "l3_blocked" / "L3 Blocked" 都等价于 "l3blocked"。
func parseRiskLevel(s string) (projecttools.RiskLevel, bool) {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', ' ', '\t':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(s)))

	switch normalized {
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
//   - ActionAllow → 直接调用原始 Execute
//   - ActionBlock → 返回 error "此操作被安全策略禁止"
//   - ActionRequireConfirm → 返回 error ErrRequireConfirm
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
