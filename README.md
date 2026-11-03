# DevAgent

运行在终端里的 AI 编程助手「小哈」，基于大模型的函数调用（Function Calling）实现，支持本地工具与 MCP（Model Context Protocol）工具。

## 功能

- **终端交互界面**：基于 Bubble Tea，支持多轮对话、滚动、输入框与加载动画。
- **本地工具**：读取文件、列出目录、获取工作目录，并带路径越界与符号链接防护。
- **MCP 支持**：通过 `mcp.json` 配置并连接任意 MCP Server，动态注册它们的工具。
- **Markdown 渲染**：把模型回答里的代码块、标题、分隔线、引用、加粗、斜体、行内代码、链接渲染成带颜色的终端样式。
- **中文助手**：内置系统提示词，让模型以「小哈」身份用中文回答。

## 快速开始

### 环境要求

- Go 1.27+
- DeepSeek API Key
- Node.js（可选，仅当 MCP 配置里用 `npx` 启动服务时需要）

### 1. 设置 API Key

```bash
# Windows (PowerShell)
$env:DEEPSEEK_API_KEY="你的 key"

# macOS / Linux
export DEEPSEEK_API_KEY="你的 key"
```

### 2. 配置 MCP（必填）

编辑项目根目录的 `mcp.json`。示例里用 `npx` 启动 `@modelcontextprotocol/server-filesystem`：

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
    }
  }
}
```

> 当前实现要求 `mcp.json` 里至少配置一个服务，否则启动会报错。

### 3. 运行

```bash
go run .
```

### 操作

| 按键 | 作用 |
| --- | --- |
| 输入 + `Enter` | 发送消息 |
| `↑` `↓` `PgUp` `PgDn` `Home` `End` | 滚动消息区 |
| `Ctrl+C` / `Esc` | 退出 |

## 项目结构

```
devagent/
├── main.go                 # 入口
├── mcp.json                # MCP Server 配置
├── agent/                  # Agent 核心
│   ├── agent.go            # 初始化、主循环、系统提示词、工具注册
│   ├── toolRun.go          # 执行单个工具、构造工具结果
│   └── mcp_adapter.go      # MCP 工具 → 本地 Tool 转换
├── mcp/                    # MCP 客户端封装
│   ├── client.go           # Manager：连接 / 列工具 / 调用 / 关闭
│   └── config.go           # 配置加载与校验
├── tools/                  # 本地工具
│   ├── tool.go             # Tool 定义与工具集合
│   ├── read_file.go        # 读取文件
│   ├── list_files.go       # 列出目录
│   ├── current_dir.go      # 获取工作目录
│   └── path.go             # 路径越界 / 符号链接校验
├── session/                # 会话历史
│   └── session.go
└── tui/                    # 终端界面
    └── model.go            # Bubble Tea UI + Markdown 渲染
```

## 工作原理

1. 用户输入 → 追加进会话历史 → 发送给模型。
2. 模型返回 `function_call` → 执行对应工具 → 把结果作为 `function_call_output` 追加回会话 → 再次请求模型。
3. 重复，直到模型不再调用工具、给出最终文本回答。

## 本地工具

| 工具 | 说明 |
| --- | --- |
| `read_file` | 读取项目根目录内的文本文件，单次最多 64KB |
| `list_files` | 列出目录内容，最多 200 项 |
| `get_current_dir` | 获取当前工作目录 |

所有本地工具都经过 `resolvePathWithin` 做越界与符号链接校验，禁止访问项目目录之外的文件。

## MCP 工具

- MCP 服务在 `mcp.json` 中配置，启动时连接并列出其工具。
- 每个 MCP 工具以 `mcp_<服务名>_<工具名>` 的前缀注册，避免与本地工具重名。
- 例如 `filesystem` 服务的 `read_file` 会注册为 `mcp_filesystem_read_file`。

## 依赖

- `charm.land/bubbletea/v2` / `charm.land/bubbles/v2` / `charm.land/lipgloss/v2` —— 终端 UI
- `github.com/openai/openai-go/v3` —— 调用 DeepSeek 大模型
- `github.com/modelcontextprotocol/go-sdk` —— MCP 客户端

## 已知限制

- Markdown 渲染是手写的轻量实现，不支持表格和嵌套格式。
- 启动时必须提供有效的 `mcp.json`（至少一个服务）。
- 本地工具只读，不具备修改文件或执行命令的能力。
