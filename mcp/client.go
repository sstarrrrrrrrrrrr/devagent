package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager 管理多个 MCP 服务端的连接。
type Manager struct {
	sessions map[string]*sdkmcp.ClientSession
}

// NewManager 创建 MCP 连接管理器。
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string]*sdkmcp.ClientSession),
	}
}

// ConnectAll 根据配置启动 MCP 服务端并建立连接。
func (m *Manager) ConnectAll(ctx context.Context, config *Config) error {
	for name, server := range config.MCPServers {
		if _, exists := m.sessions[name]; exists {
			continue
		}

		cmd := exec.Command(server.Command, server.Args...)

		// 继承当前进程的环境变量，并追加配置文件中的变量。
		if len(server.Env) > 0 {
			cmd.Env = os.Environ()
			for key, value := range server.Env {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}

		client := sdkmcp.NewClient(
			&sdkmcp.Implementation{
				Name:    "devagent",
				Version: "1.0.0",
			},
			nil,
		)

		transport := &sdkmcp.CommandTransport{
			Command: cmd,
		}

		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return fmt.Errorf("连接 MCP 服务 %q 失败: %w", name, err)
		}

		m.sessions[name] = session
	}

	return nil
}

// ListTools 获取指定 MCP 服务端提供的工具列表。
func (m *Manager) ListTools(
	ctx context.Context,
	serverName string,
) ([]*sdkmcp.Tool, error) {
	session, exists := m.sessions[serverName]
	if !exists {
		return nil, fmt.Errorf("MCP 服务 %q 尚未连接", serverName)
	}

	result, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("获取 MCP 工具列表失败: %w", err)
	}

	return result.Tools, nil
}

// Close 关闭所有 MCP 会话。
func (m *Manager) Close() {
	for name, session := range m.sessions {
		if err := session.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "关闭 MCP 服务 %q 失败: %v\n", name, err)
		}
		delete(m.sessions, name)
	}
}

// CallTool 调用指定 MCP 服务端的工具。
func (m *Manager) CallTool(
	ctx context.Context,
	serverName string,
	toolName string,
	arguments any,
) (*sdkmcp.CallToolResult, error) {
	session, exists := m.sessions[serverName]
	if !exists {
		return nil, fmt.Errorf("MCP 服务 %q 尚未连接", serverName)
	}
	params := &sdkmcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	}
	result, err := session.CallTool(ctx, params)
	if err != nil {
		return nil, fmt.Errorf(
			"调用 MCP 工具 %q 失败: %w",
			toolName,
			err,
		)
	}
	return result, nil
}

// ExtractText 从 MCP 工具执行结果中提取文本。
func ExtractText(result *sdkmcp.CallToolResult) (string, error) {
	if result == nil {
		return "", fmt.Errorf("MCP 工具返回结果为空")
	}

	if result.IsError {
		return "", fmt.Errorf("MCP 工具执行失败")
	}

	var output strings.Builder

	for _, item := range result.Content {
		switch content := item.(type) {
		case *sdkmcp.TextContent:
			output.WriteString(content.Text)
			output.WriteString("\n")

		case *sdkmcp.ImageContent:
			output.WriteString(
				fmt.Sprintf("[图片内容，类型: %s]\n", content.MIMEType),
			)

		default:
			output.WriteString("[暂不支持的内容类型]\n")
		}
	}

	return output.String(), nil
}
