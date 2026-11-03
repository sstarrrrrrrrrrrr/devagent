package agent

import (
	"context"
	"encoding/json"
	"fmt"

	devmcp "devagent/mcp"
	projecttools "devagent/tools"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaToMap 将 MCP 工具的 InputSchema 转换为项目需要的参数格式。
func schemaToMap(schema any) (map[string]any, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("序列化 MCP 参数 Schema 失败: %w", err)
	}

	var params map[string]any
	if err := json.Unmarshal(data, &params); err != nil {
		return nil, fmt.Errorf("解析 MCP 参数 Schema 失败: %w", err)
	}

	return params, nil
}

// buildMCPTools 将 MCP Server 提供的工具转换为 Agent 可以使用的工具。
func buildMCPTools(
	manager *devmcp.Manager,
	serverName string,
	mcpTools []*sdkmcp.Tool,
) ([]projecttools.Tool, error) {

	var result []projecttools.Tool

	for _, mcpTool := range mcpTools {
		// 保存当前 MCP 工具名称
		originalToolName := mcpTool.Name

		parameters, err := schemaToMap(mcpTool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf(
				"转换 MCP 工具 %s 的参数 Schema 失败: %w",
				originalToolName,
				err,
			)
		}

		// 给 MCP 工具添加前缀，避免与本地工具重名
		agentToolName := "mcp_" + serverName + "_" + originalToolName

		tool := projecttools.Tool{
			Name:        agentToolName,
			Description: mcpTool.Description,
			Parameters:  parameters,

			Execute: func(
				ctx context.Context,
				args map[string]any,
			) (string, error) {

				callResult, err := manager.CallTool(
					ctx,
					serverName,
					originalToolName,
					args,
				)
				if err != nil {
					return "", err
				}

				return devmcp.ExtractText(callResult)
			},
		}

		result = append(result, tool)
	}

	return result, nil
}
