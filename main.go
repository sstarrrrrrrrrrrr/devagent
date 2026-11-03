package main

import (
	"devagent/tui"
	"fmt"
)

func main() {
	if err := tui.Run(); err != nil {
		fmt.Println("启动 TUI 失败：", err)
	}
}
