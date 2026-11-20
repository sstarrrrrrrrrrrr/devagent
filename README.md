# DevAgent

运行在终端里的 AI 编程助手「小哈」，基于大模型的函数调用（Function Calling）实现，支持本地工具、MCP（Model Context Protocol）工具与安全审批框架。

## 功能

- **终端交互界面**：基于 Bubble Tea，支持多轮对话、滚动、输入框与加载动画。
- **本地工具**：读取文件、列出目录、获取工作目录、执行 shell 命令，带路径越界与符号链接防护。
- **MCP 支持**：通过 `mcp.json` 配置并连接任意 MCP Server，支持 stdio（本地子进程）、SSE 与 Streamable HTTP（远程）三种传输方式，动态注册它们的工具。
- **连接生命周期治理**：断线自动标记、每次运行前自动重连、部署新版后工具列表/schema 刷新。
- **安全审批框架**：按「只读 / 写入 / 删除 / 执行」对工具语义分级，危险操作在 TUI 里弹出确认框，支持黑名单（工具/路径/命令 pattern）与白名单。
- **Markdown 渲染**：把模型回答里的代码块、标题、分隔线、引用、加粗、斜体、行内代码、链接渲染成带颜色的终端样式。
- **中文助手**：内置系统提示词，让模型以「小哈」身份用中文回答。

## 快速开始

### 环境要求

- Go 1.27+
- DeepSeek API Key
- Node.js（可选，仅当 MCP 配置里用 `npx` 启动服务时需要）
- Docker / 本地编译的 MCP Server 二进制（可选，stdio 模式需要）

### 1. 设置 API Key

```bash
# Windows (PowerShell)
$env:DEEPSEEK_API_KEY="你的 key"

# macOS / Linux
export DEEPSEEK_API_KEY="你的 key"
```

### 2. 配置 MCP（必填）

编辑项目根目录的 `mcp.json`。支持三种传输方式：

**stdio（本地子进程）** —— 通过 stdin/stdout 与本地 MCP 进程通信：

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
      "timeout": "30s",
      "retry": 1
    }
  }
}
```

**streamable（远程 Streamable HTTP）** —— 连接 MCP 2025-03 规范的远程服务端，如 GitHub 官方远程 MCP：

```json
{
  "mcpServers": {
    "github": {
      "transport": "streamable",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "Authorization": "Bearer ${GITHUB_TOKEN}"
      },
      "timeout": "30s",
      "callTimeout": "60s",
      "retry": 2
    }
  }
}
```

**sse（远程 HTTP + SSE）** —— 连接传统 SSE 服务端（先挂长连接再 POST 消息）：

```json
{
  "mcpServers": {
    "remote": {
      "transport": "sse",
      "url": "https://example.com/mcp/sse"
    }
  }
}
```

通用可选字段：`timeout`（连接超时，默认 30s）、`callTimeout`（单次工具调用超时，默认 60s）、`retry`（连接重试次数）。

> 当前实现要求 `mcp.json` 里至少配置一个服务，否则启动会报错。

### 3. 设置 MCP 环境变量

`headers` 与 `env` 的值支持 `${VAR}` 引用环境变量（如上面的 `${GITHUB_TOKEN}`），**推荐 token 类敏感信息走环境变量而不是明文写进配置文件**。GitHub PAT 从 https://github.com/settings/tokens 生成：

```bash
# Windows (PowerShell)
$env:GITHUB_TOKEN="github_pat_xxx"

# macOS / Linux
export GITHUB_TOKEN="github_pat_xxx"
```

### 4. 运行

```bash
go run .
```

### 操作

| 按键 | 作用 |
| --- | --- |
| 输入 + `Enter` | 发送消息 |
| `↑` `↓` `PgUp` `PgDn` `Home` `End` | 滚动消息区 |
| `Y` / `R` / `A` / `N` | 审批弹窗：允许 / 拒绝 / 全部允许 / 全部拒绝 |
| `Ctrl+C` / `Esc` | 退出 |

## 项目结构

```
devagent/
├── main.go                 # 入口
├── mcp.json                # MCP Server 配置
├── agent/                  # Agent 核心
│   ├── agent.go            # 初始化、主循环、系统提示词、工具注册
│   ├── toolRun.go          # 执行单个工具、处理确认、构造工具结果
│   └── mcp_adapter.go      # MCP 工具 → 本地 Tool 转换
├── mcp/                    # MCP 客户端封装
│   ├── client.go           # Manager：连接 / 重连 / 列工具 / 调用 / 关闭
│   └── config.go           # 配置加载与校验（三种传输、${VAR} 环境变量展开）
├── tools/                  # 本地工具
│   ├── tool.go             # Tool 定义、风险等级、工具集合
│   ├── read_file.go        # 读取文件
│   ├── list_files.go       # 列出目录
│   ├── current_dir.go      # 获取工作目录
│   ├── run_command.go      # 执行 shell 命令（带超时与输出截断）
│   └── path.go             # 路径越界 / 符号链接校验
├── safety/                 # 安全审批框架
│   ├── config.go           # 策略加载（默认黑名单 + 用户 safety.json）
│   └── guard.go            # 决策链：黑名单 → 白名单 → 覆盖 → 语义分类
├── session/                # 会话历史
│   └── session.go
└── tui/                    # 终端界面
    └── model.go            # Bubble Tea UI + Markdown 渲染 + 审批弹窗
```

## 工作原理

1. 用户输入 → 追加进会话历史 → 发送给模型。
2. 模型返回 `function_call` → SafetyGuard 对这次调用做决策（放行 / 需确认 / 拒绝）→ 需确认时暂停并弹窗等用户决策 → 执行对应工具 → 把结果作为 `function_call_output` 追加回会话 → 再次请求模型。
3. 重复，直到模型不再调用工具、给出最终文本回答。
4. MCP 服务挂掉时自动标记为 dead，下次运行前自动尝试重连。

## 本地工具

| 工具 | 风险等级 | 说明 |
| --- | --- | --- |
| `read_file` | L0-Safe | 读取项目根目录内的文本文件，单次最多 64KB |
| `list_files` | L0-Safe | 列出目录内容，最多 200 项 |
| `get_current_dir` | L0-Safe | 获取当前工作目录 |
| `run_command` | L2-Review | 在项目根目录执行 shell 命令（git、构建、测试等），每次执行前需用户审核 |

只读工具经过 `resolvePathWithin` 做越界与符号链接校验，禁止访问项目目录之外的文件。`run_command` 每次调用都要经过审批确认。

## 安全审批框架

工具按语义自动分级，SafetyGuard 在每次工具调用前拦截判定：

| 等级 | 行为 | 典型工具 |
| --- | --- | --- |
| L0-Safe | 直接放行 | 只读工具（项目目录内） |
| L1-Confirm | 需要用户确认 | 目录外读取、用途未知 |
| L2-Review | 需要用户审核 | 写文件、删除、执行命令 |
| L3-Blocked | 直接拒绝 | 命中黑名单 |

判定优先级：黑名单（工具名 / 路径 / 命令 pattern）→ 白名单 → 用户配置覆盖（`riskOverrides`）→ 按工具名关键词语义分类。

默认黑名单内置系统关键路径（`C:\Windows\System32`、`~/.ssh` 等）与危险命令 pattern（`rm -rf /`、`format`、`shutdown`、`sudo` 等）。可通过项目根目录的 `safety.json` 自定义（文件不存在时使用默认策略）：

```json
{
  "blocked": {
    "commandPatterns": ["git\\s+push\\s+--force"]
  },
  "whitelisted": {
    "tools": ["get_current_dir"]
  }
}
```

## MCP 工具

- MCP 服务在 `mcp.json` 中配置，启动时连接并列出其工具。
- 每个 MCP 工具以 `mcp_<服务名>_<工具名>` 的前缀注册，避免与本地工具重名。
- 例如 `filesystem` 服务的 `read_file` 会注册为 `mcp_filesystem_read_file`。
- MCP 工具统一按 L1-Confirm 占位，实际等级由 SafetyGuard 按语义（写/删/执行关键词）动态推断。

## 依赖

- `charm.land/bubbletea/v2` / `charm.land/bubbles/v2` / `charm.land/lipgloss/v2` —— 终端 UI
- `github.com/openai/openai-go/v3` —— 调用 DeepSeek 大模型
- `github.com/modelcontextprotocol/go-sdk` —— MCP 客户端（stdio / SSE / Streamable HTTP）

## 已知限制

- Markdown 渲染是手写的轻量实现，不支持表格和嵌套格式。
- 启动时必须提供有效的 `mcp.json`（至少一个服务）。
- MCP 工具的风险等级靠名称关键词推断，工具名不含特征词时保守按 L1 处理。
- `AliveServerSync` 目前只比较工具名称，schema 变化检测待优化。
