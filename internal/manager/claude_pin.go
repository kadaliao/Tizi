package manager

import (
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
)

// Mihomo 会把每个策略组的选择记在 cache.db 里，重启后恢复。临时把 Claude
// 改走其他节点以后，不手动切回的话，重新连接、重载甚至重启电脑后 Claude
// 仍走那个节点。所以每次启动后都把「Claude」组切回住宅节点。
//
// Mihomo 先开放 Controller，之后才恢复记下的选择，只切一次可能被它盖掉：
// 第一次看到加载好的「Claude」组后，在 claudePinWindow 内一直盯住它。
const (
	// claudePinWait 是等 Mihomo 加载完配置的最长时间。
	claudePinWait     = 60 * time.Second
	claudePinWindow   = 15 * time.Second
	claudePinInterval = 500 * time.Millisecond
)

type groupSelector interface {
	Groups() ([]controller.Group, error)
	Select(group, choice string) error
}

// startClaudePin 在后台把「Claude」组切到住宅节点；没有配置住宅节点时什么
// 也不做。返回的函数让它提前结束。
func startClaudePin(generated string, logWriter io.Writer) func() {
	client, err := controllerFromGeneratedConfig(generated)
	if err != nil {
		return func() {}
	}
	stop := make(chan struct{})
	go pinClaudeResidential(stop, client, logWriter, claudePinWait, claudePinWindow, claudePinInterval)
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

func pinClaudeResidential(stop <-chan struct{}, client groupSelector, logWriter io.Writer, wait, window, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.Now().Add(wait)
	loaded := false
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return
		}
		groups, err := client.Groups()
		if err != nil {
			continue
		}
		index := slices.IndexFunc(groups, func(group controller.Group) bool { return group.Name == config.ClaudeGroup })
		if index < 0 {
			// Controller 已经能访问，但配置还没加载完。
			continue
		}
		group := groups[index]
		if !slices.Contains(group.Choices, config.ResidentialProxyName) {
			return
		}
		if !loaded {
			loaded = true
			deadline = time.Now().Add(window)
		}
		if group.Now == config.ResidentialProxyName {
			continue
		}
		if err := client.Select(config.ClaudeGroup, config.ResidentialProxyName); err != nil {
			_, _ = fmt.Fprintf(logWriter, "把 Claude 切回%s: %v\n", config.ResidentialProxyName, err)
			continue
		}
		_, _ = fmt.Fprintf(logWriter, "Claude 已切回%s（Mihomo 恢复的上次选择是 %s）\n", config.ResidentialProxyName, group.Now)
	}
}
