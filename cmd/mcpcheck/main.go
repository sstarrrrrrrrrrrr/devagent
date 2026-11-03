package main

import (
	"context"
	"fmt"
	"log"
	"time"

	devmcp "devagent/mcp"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		60*time.Second,
	)
	defer cancel()

	// 1. 加载 MCP 配置
	config, err := devmcp.LoadConfig("mcp.json")
	if err != nil {
		return fmt.Errorf("加载 MCP 配置失败: %w", err)
	}

	// 2. 创建 MCP 管理器
	manager := devmcp.NewManager()
	defer manager.Close()

	// 3. 连接所有 MCP Server
	if err := manager.ConnectAll(ctx, config); err != nil {
		return fmt.Errorf("连接 MCP 服务失败: %w", err)
	}

	fmt.Println("MCP 服务连接成功！")

	// 4. 获取 MCP 工具列表
	tools, err := manager.ListTools(ctx, "filesystem")
	if err != nil {
		return fmt.Errorf("获取工具列表失败: %w", err)
	}

	fmt.Printf("共发现 %d 个工具\n", len(tools))

	// 5. 调用 list_directory 工具
	result, err := manager.CallTool(
		ctx,
		"filesystem",
		"list_directory",
		map[string]any{
			"path": ".",
		},
	)
	for _, tool := range tools {
		if tool.Name == "list_directory" {
			fmt.Printf("工具名称：%s\n", tool.Name)
			fmt.Printf("参数 Schema 类型：%T\n", tool.InputSchema)
			fmt.Printf("参数 Schema 内容：%#v\n", tool.InputSchema)
		}
	}
	if err != nil {
		return fmt.Errorf("调用 MCP 工具失败: %w", err)
	}

	// 6. 解析工具返回结果
	output, err := devmcp.ExtractText(result)
	if err != nil {
		return fmt.Errorf("解析 MCP 工具结果失败: %w", err)
	}

	// 7. 打印工具执行结果
	fmt.Println("\n项目目录内容：")
	fmt.Println(output)

	return nil
}
