package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config 是 mcp.json 的顶层结构。
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// 支持的传输类型。
const (
	TransportStdio = "stdio"
	TransportSSE   = "sse"
)

// ServerConfig 描述一个 MCP Server 的连接方式。
//
// 根据 Transport 字段选择不同的子配置：
//   - "stdio"（默认）：Command + Args + Env，本地起子进程走 stdin/stdout
//   - "sse"：URL + Headers，连接远程 SSE 服务端
type ServerConfig struct {
	// Transport 决定传输方式，留空或 "stdio" 为子进程模式，"sse" 为远程 SSE。
	Transport string `json:"transport,omitempty"`

	// ---- stdio 专用字段 ----
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// ---- sse 专用字段 ----
	URL     string           `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// ---- 通用可选字段 ----
	// Timeout 单次连接超时（connectOne 用），留空则用默认 30s。
	Timeout Duration `json:"timeout,omitempty"`
	// CallTimeout 单次工具调用/ListTools 超时，留空则用默认 60s。
	CallTimeout Duration `json:"callTimeout,omitempty"`
	// Retry 连接失败后的最大重试次数，留空为 0（不重试）。
	Retry int `json:"retry,omitempty"`
}

// Duration 让 JSON 里能用 "30s" 这种字符串表达时长。
type Duration struct {
	time.Duration
}

// UnmarshalJSON 把 JSON 数字（毫秒）或字符串（如 "30s"）解析为 Duration。
func (d *Duration) UnmarshalJSON(data []byte) error {
	// 尝试字符串形式
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		td, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("无效的时长字符串 %q: %w", s, err)
		}
		d.Duration = td
		return nil
	}

	// 尝试数字形式（毫秒）
	var ms float64
	if err := json.Unmarshal(data, &ms); err != nil {
		return fmt.Errorf("无法解析时长: %s", string(data))
	}
	d.Duration = time.Duration(ms * float64(time.Millisecond))
	return nil
}

// GetTransport 返回归一化后的传输类型，空值视为 stdio。
func (s ServerConfig) GetTransport() string {
	if s.Transport == "" {
		return TransportStdio
	}
	return s.Transport
}

// GetTimeout 返回连接超时，未设置时用默认值 30s。
func (s ServerConfig) GetTimeout() time.Duration {
	if s.Timeout.Duration > 0 {
		return s.Timeout.Duration
	}
	return 30 * time.Second
}

// GetCallTimeout 返回单次工具调用/ListTools 超时，未设置时用默认值 60s。
func (s ServerConfig) GetCallTimeout() time.Duration {
	if s.CallTimeout.Duration > 0 {
		return s.CallTimeout.Duration
	}
	return 60 * time.Second
}

// LoadConfig 从 JSON 文件中读取 MCP 配置并校验。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 MCP 配置文件失败: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析 MCP 配置文件失败: %w", err)
	}

	if len(config.MCPServers) == 0 {
		return nil, fmt.Errorf("MCP 配置中没有任何服务")
	}

	for name, server := range config.MCPServers {
		if err := validateServer(name, server); err != nil {
			return nil, err
		}
	}

	return &config, nil
}

// validateServer 按传输类型分别校验必填字段。
func validateServer(name string, server ServerConfig) error {
	switch server.GetTransport() {
	case TransportStdio:
		if server.Command == "" {
			return fmt.Errorf("MCP 服务 %q (stdio) 缺少 command 字段", name)
		}
	case TransportSSE:
		if server.URL == "" {
			return fmt.Errorf("MCP 服务 %q (sse) 缺少 url 字段", name)
		}
	default:
		return fmt.Errorf("MCP 服务 %q 使用了未知传输类型 %q，支持: %s, %s",
			name, server.Transport, TransportStdio, TransportSSE)
	}
	return nil
}
