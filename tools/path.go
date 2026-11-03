package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// 返回解析后的真实绝对路径。
func resolvePathWithin(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析项目根目录失败: %w", err)
	}

	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(rootAbs, target)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("解析目标路径失败: %w", err)
	}

	// EvalSymlinks 会解析符号链接，同时也会解析 .. 这类相对跳转，
	// 所以只需在解析后再做一次越界判断即可。
	realRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("解析项目根目录链接失败: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return "", fmt.Errorf("解析目标路径失败，文件可能不存在: %w", err)
	}

	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return "", fmt.Errorf("检查路径失败: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("拒绝访问项目目录之外的路径")
	}

	return realTarget, nil
}
