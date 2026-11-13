package safety

import (
	"encoding/json"
	"fmt"
	"os"
)

// SafetyConfig 是安全策略的顶层结构，对应 safety.json。
//
// 拦截器链的处理顺序（优先级从高到低）：
//  1. Blocklist 命中 → 直接 L3-Blocked（拒绝）
//  2. Whitelist 命中 → 跳过所有后续检查，直接放行（保留工具原始 RiskLevel）
//  3. RiskLevel 策略 → 工具自己声明的基础等级
//  4. 参数级规则 → 根据路径/命令等参数动态升级或降级
//  5. 兜底 → 未知工具默认 L1-Confirm
type SafetyConfig struct {
	// Workdir 指定工作目录根路径，用于路径沙箱。留空则自动取 os.Getwd()。
	Workdir string `json:"workdir,omitempty"`

	// Blocklist 硬黑名单，一旦命中直接拒绝。
	Blocklist BlocklistConfig `json:"blocked"`

	// Whitelist 白名单，一旦命中跳过所有检查直接放行。
	Whitelist WhitelistConfig `json:"whitelisted"`

	// RiskOverrides 工具级 RiskLevel 覆盖，key 是工具名 glob pattern。
	// 例如 "mcp_*delete*" → L3-Blocked。
	RiskOverrides map[string]string `json:"riskOverrides,omitempty"`

	// DefaultRisk 未匹配任何策略的工具默认等级，留空用 L1-Confirm。
	DefaultRisk string `json:"defaultRisk,omitempty"`
}

// BlocklistConfig 黑名单配置，任何一项命中都触发 L3-Blocked。
type BlocklistConfig struct {
	// Paths 路径 glob pattern，匹配参数里的 path/filePath/target 等字段。
	// 例: "/etc/*", "~/.ssh/*", ".git/*", "C:\\Windows\\System32\\*"
	Paths []string `json:"paths,omitempty"`

	// CommandPatterns 命令字段 regex pattern，匹配参数里的 command/cmd/shell 等字段。
	// 例: "rm -rf /", "format C:", "shutdown"
	CommandPatterns []string `json:"commandPatterns,omitempty"`

	// Tools 工具名 glob pattern，例: "mcp_*_*delete*", "mcp_*_*execute*"
	Tools []string `json:"tools,omitempty"`
}

// WhitelistConfig 白名单配置，任何一项命中都跳过所有检查直接放行。
type WhitelistConfig struct {
	// Paths 路径 glob pattern。
	Paths []string `json:"paths,omitempty"`

	// Tools 工具名 glob pattern。
	Tools []string `json:"tools,omitempty"`
}

// LoadSafetyConfig 从 JSON 文件加载安全策略。
// 文件不存在时返回默认安全策略（拒绝极危险路径/命令），不会报错。
func LoadSafetyConfig(path string) (*SafetyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在 → 返回默认配置（仍然保护基础危险路径）
		if os.IsNotExist(err) {
			return defaultSafetyConfig(), nil
		}
		return nil, fmt.Errorf("读取安全配置失败: %w", err)
	}

	var config SafetyConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析安全配置失败: %w", err)
	}

	// 合并默认值：即使配置里没写，也强制保护系统关键路径
	config.Blocklist.Paths = append(
		config.Blocklist.Paths,
		defaultBlockedPaths...,
	)
	config.Blocklist.CommandPatterns = append(
		config.Blocklist.CommandPatterns,
		defaultBlockedCommands...,
	)

	return &config, nil
}

// defaultSafetyConfig 当 safety.json 不存在时使用的策略。
func defaultSafetyConfig() *SafetyConfig {
	return &SafetyConfig{
		Blocklist: BlocklistConfig{
			Paths:           defaultBlockedPaths,
			CommandPatterns: defaultBlockedCommands,
			Tools:           []string{},
		},
		Whitelist:     WhitelistConfig{},
		RiskOverrides: map[string]string{},
		DefaultRisk:   "L1-Confirm",
	}
}

// defaultBlockedPaths 默认拦截的路径 glob pattern（即使配置文件不存在也生效）。
var defaultBlockedPaths = []string{
	"/etc/*",
	"/proc/*",
	"/sys/*",
	"/dev/*",
	"/boot/*",
	"/root/*",
	"~/.ssh/*",
	"~/.aws/*",
	"~/.config/gcloud/*",
	"~/.kube/*",
	"~/.docker/*",
	"C:\\Windows\\System32\\*",
	"C:\\Windows\\WinSxS\\*",
	"C:\\Windows\\Boot\\*",
	"C:\\ProgramData\\*",
	".git/*",
}

// defaultBlockedCommands 默认拦截的命令 regex pattern。
var defaultBlockedCommands = []string{
	`(?i)rm\s+-rf\s+/`,
	`(?i)rm\s+-rf\s+~`,
	`(?i)rm\s+-rf\s+\*`,
	`(?i)format\s+[A-Za-z]:`,
	`(?i)shutdown(\s|$)`,
	`(?i)reboot(\s|$)`,
	`(?i)poweroff(\s|$)`,
	`(?i)halt(\s|$)`,
	`(?i)sudo(\s|$)`,
	`(?i)pkexec(\s|$)`,
	`(?i)chmod\s+\+s`,
	`(?i)chown\s+root`,
}
