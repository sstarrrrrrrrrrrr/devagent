package main

import (
	"devagent/tui"
	"fmt"
)

//1111111
//2222
func main() {
	if err := tui.Run(); err != nil {
		fmt.Println("启动 TUI 失败：", err)
	}
}
