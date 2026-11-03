package mcp

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是 mcp.json 的顶层结构。
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig 描述一个 MCP Server 的启动方式。
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// LoadConfig 从 JSON 文件中读取 MCP 配置。
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
		if server.Command == "" {
			return nil, fmt.Errorf("MCP 服务 %q 没有配置 command", name)
		}
	}

	return &config, nil
}
