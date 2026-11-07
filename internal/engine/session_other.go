//go:build !darwin

package engine

import "fmt"

// RunInTerminalSession 只在 macOS 上提供；图形界面目前只有 macOS 版。
func RunInTerminalSession(args []string) error {
	return fmt.Errorf("图形界面后台引擎仅支持 macOS")
}

func PrepareSessionChild() {}
