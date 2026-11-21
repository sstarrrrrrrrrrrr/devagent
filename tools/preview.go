package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// previewServer 是 open_preview 启动的本地静态服务。
// 同一个目录重复预览时复用已有服务，避免每次调用都新增端口占用。
type previewServer struct {
	url string
	srv *http.Server
}

// previewServers 记录已启动的预览服务：服务根目录 → 服务实例。
// 只在 Execute 里读写，TUI 的工具调用是串行的，无需加锁。
var previewServers = make(map[string]*previewServer)

// OpenPreviewTool 在本地起静态文件服务并用默认浏览器打开指定页面。
// 用于前端页面写完后的渲染预览，形成「生成 → 预览 → 反馈 → 修改」循环。
var OpenPreviewTool = Tool{
	Name:        "open_preview",
	Description: "在本地启动一个静态文件服务器，并用系统默认浏览器打开指定网页文件进行预览。常用于生成 HTML 页面后查看渲染效果",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要预览的 HTML 文件路径（项目内相对路径或绝对路径），也可以是目录（浏览目录列表）",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	},
	// 本地回环服务 + 弹浏览器，无破坏性，但涉及启动进程 → L1 确认
	RiskLevel:    L1Confirm,
	ExplicitRisk: true,
	Execute: func(ctx context.Context, args map[string]any) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		pathValue, ok := args["path"].(string)
		if !ok || strings.TrimSpace(pathValue) == "" {
			return "", fmt.Errorf("path 参数必须是非空字符串")
		}

		// 路径沙箱：只允许预览项目目录内的文件
		root := workdirRoot()
		target, err := resolvePathWithin(root, pathValue)
		if err != nil {
			return "", err
		}

		// 服务根目录：目标是文件 → 其所在目录；目标是目录 → 本身
		serveRoot := target
		if info, statErr := os.Stat(target); statErr == nil && !info.IsDir() {
			serveRoot = filepath.Dir(target)
		}

		ps, err := ensurePreviewServer(serveRoot)
		if err != nil {
			return "", err
		}

		// 要打开的 URL：文件拼相对路径，目录直接开根
		openURL := ps.url
		if rel, relErr := filepath.Rel(serveRoot, target); relErr == nil && rel != "." {
			openURL += "/" + filepath.ToSlash(rel)
		}

		if err := openBrowser(openURL); err != nil {
			return "", fmt.Errorf("打开浏览器失败: %w（页面地址: %s，可手动打开）", err, openURL)
		}

		return fmt.Sprintf("预览服务已启动: %s\n已在默认浏览器中打开。服务在 agent 退出后自动停止。", openURL), nil
	},
}

// ensurePreviewServer 对同一服务根目录复用已启动的服务，否则起新的。
func ensurePreviewServer(serveRoot string) (*previewServer, error) {
	if ps, ok := previewServers[serveRoot]; ok {
		return ps, nil
	}

	// :0 让系统分配空闲端口，规避端口冲突
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("启动本地预览服务失败: %w", err)
	}

	ps := &previewServer{
		url: fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port),
		srv: &http.Server{Handler: http.FileServer(http.Dir(serveRoot))},
	}
	go func() {
		_ = ps.srv.Serve(ln)
	}()

	previewServers[serveRoot] = ps
	return ps, nil
}

// openBrowser 调系统默认浏览器打开 URL。
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		// start 的第一个引号参数是窗口标题，占位空串避免 URL 被当成标题
		return exec.Command("cmd", "/c", "start", "", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
