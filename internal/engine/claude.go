package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeblock"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/nodes"
	"github.com/kadaliao/tizi/internal/system"
)

// claudeCheckTimeout 限定一次检测的总时间。
const claudeCheckTimeout = 30 * time.Second

// claudeLoop 检测 Claude 的线路，结论随快照推给界面，由界面弹窗。除了定期
// 检测，Mihomo 启停（kickClaude）和日志里 Claude 的失败、直连
// （claudeEvents）都会立即触发。它同时维持心跳文件，告诉后台的 Mihomo
// 启动器桌面版在负责告警；引擎退出时删掉心跳，启动器立即接手。
func (e *Engine) claudeLoop(ctx context.Context) {
	defer claudeguard.RemoveHeartbeat(e.paths.ConfigDir)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-e.claudeKick:
		case event := <-e.claudeEvents:
			if !e.handleClaudeEvent(event) {
				continue
			}
		}
		timer.Stop()
		_ = claudeguard.TouchHeartbeat(e.paths.ConfigDir)
		e.checkClaude(ctx)
		e.claudeMu.Lock()
		e.claudeTracker.Done(time.Now())
		delay := e.claudeTracker.NextDelay()
		e.claudeMu.Unlock()
		timer.Reset(delay)
	}
}

// handleClaudeEvent 处理一条日志事件，返回 true 表示要立即检测。
func (e *Engine) handleClaudeEvent(event claudeguard.LogEvent) bool {
	if !claudeguard.Enabled(e.paths.ConfigDir, e.currentSettings()) {
		return false
	}
	e.claudeMu.Lock()
	probe := e.claudeTracker.Event(event, time.Now())
	e.claudeMu.Unlock()
	if event.Direct {
		e.requestRefresh()
	}
	return probe
}

// claudeLogLoop 在开启监测、Mihomo 运行时订阅它的实时日志，把与 Claude
// 有关的失败和直连交给 claudeLoop。连接断开（Mihomo 重启）后重新订阅。
func (e *Engine) claudeLogLoop(ctx context.Context) {
	for {
		if claudeguard.Enabled(e.paths.ConfigDir, e.currentSettings()) && e.manager.Status().Running {
			if runtime, err := claudeguard.ReadRuntime(e.paths.Generated); err == nil {
				_ = runtime.Client.StreamLogs(ctx, "info", func(_, payload string) {
					if event, ok := claudeguard.ParseLog(payload); ok {
						select {
						case e.claudeEvents <- event:
						default:
						}
					}
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(claudeLogRetry):
		}
	}
}

// claudeLogRetry 是日志订阅断开后重新订阅前的等待。
const claudeLogRetry = 3 * time.Second

// kickClaude 让 claudeLoop 立即检测；reset 为真时丢掉之前的结论和累计的
// 失败，用于用户刚改变了线路（连接、断开、切换策略）之后。
func (e *Engine) kickClaude(reset bool) {
	if reset {
		e.claudeMu.Lock()
		e.claudeTracker.Reset()
		e.claudeMu.Unlock()
	}
	select {
	case e.claudeKick <- struct{}{}:
	default:
	}
}

func (e *Engine) checkClaude(ctx context.Context) claudeguard.Status {
	settings := e.currentSettings()
	e.mu.Lock()
	busy := e.busy
	e.mu.Unlock()
	if busy != "" && claudeguard.Enabled(e.paths.ConfigDir, settings) {
		// 连接、断开进行中，线路本来就在变；操作结束后会立即重新检测。
		e.claudeMu.Lock()
		defer e.claudeMu.Unlock()
		return e.claudeTracker.Status()
	}
	env := e.manager.ClaudeEnv(settings)
	ctx, cancel := context.WithTimeout(ctx, claudeCheckTimeout)
	defer cancel()
	observed := e.observeClaude(ctx, env)
	e.claudeMu.Lock()
	status := e.claudeTracker.Update(observed)
	e.claudeMu.Unlock()
	e.requestRefresh()
	return status
}

func (e *Engine) claudeView() Claude {
	e.claudeMu.Lock()
	status := e.claudeTracker.Status()
	e.claudeMu.Unlock()
	view := Claude{
		Level:       status.Level,
		Key:         status.Key,
		Title:       status.Title,
		Detail:      status.Detail,
		Residential: status.Residential,
		Route:       status.Route,
		ExitIP:      status.ExitIP,
		Country:     status.Country,
		Pending:     status.Pending,
	}
	if !status.CheckedAt.IsZero() {
		view.CheckedAt = status.CheckedAt.UnixMilli()
	}
	return view
}

// addClaudeSettings 填入住宅节点相关的设置；地址里的密码不离开引擎。
func (e *Engine) addClaudeSettings(view *Settings, settings config.Settings) {
	view.ClaudeProxy = claude.MaskURL(settings.ClaudeProxy)
	if node, err := claude.SplitURL(settings.ClaudeProxy); err == nil {
		view.ClaudeNode = &ClaudeNode{
			Scheme:      node.Scheme,
			Host:        node.Host,
			Port:        node.Port,
			Username:    node.Username,
			HasPassword: node.Password != "",
		}
	}
	view.ClaudeProxyDirect = settings.ClaudeProxyDirect
	view.ClaudeGuard = settings.ClaudeGuard
	view.ClaudeGuardActive = claudeguard.Enabled(e.paths.ConfigDir, settings)
	view.ClaudeProxyInFile = strings.TrimSpace(settings.ClaudeProxy) == "" && claude.Configured(e.paths.ConfigDir, settings)
	if _, err := claude.Load(e.paths.ConfigDir, settings); err != nil {
		view.ClaudeProxyError = err.Error()
	}
	view.ClaudeBlockSupported = claudeblock.Supported()
	view.ClaudeBlock = claudeblock.Installed()
}

// setClaudeBlock 安装或卸载直连保护，需要管理员授权。
func (e *Engine) setClaudeBlock(enabled bool) (any, error) {
	if !claudeblock.Supported() {
		return nil, failure(CodeFailed, "直连保护目前只支持 macOS")
	}
	if err := system.AuthorizeAdministrator(); err != nil {
		return nil, err
	}
	if !enabled {
		if err := claudeblock.Remove(); err != nil {
			return nil, err
		}
		return ActionResult{Message: "直连保护已关闭：VPN 断开时访问 Claude 会直连"}, nil
	}
	if err := claudeblock.Install(); err != nil {
		return nil, err
	}
	if err := claudeblock.SelfTest(); err != nil && !errors.Is(err, claudeblock.ErrSelfTestOffline) {
		return nil, fmt.Errorf("直连保护已安装，但自检没有通过：%w", err)
	}
	return ActionResult{Message: "直连保护已开启：不经 VPN 的 Claude 连接会被拦截，重启后依然有效"}, nil
}

func (e *Engine) claudeProxyPath() string {
	return filepath.Join(e.paths.ConfigDir, config.ClaudeProxyFile)
}

// ensureClaudeProxyFile 返回住宅节点文件的路径；文件不存在时先写入只有注释的说明。
func (e *Engine) ensureClaudeProxyFile() (any, error) {
	path := e.claudeProxyPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := e.paths.Ensure(); err != nil {
			return nil, err
		}
		if err := config.WriteFileAtomic(path, []byte(claude.FileTemplate), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return path, nil
}

func (e *Engine) proxiesPath() string {
	if e.paths.Proxies != "" {
		return e.paths.Proxies
	}
	return filepath.Join(e.paths.ConfigDir, config.ProxiesFile)
}

// ensureProxiesFile 返回节点文件的路径；文件不存在时先写入只有注释的说明。
func (e *Engine) ensureProxiesFile() (any, error) {
	path := e.proxiesPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := e.paths.Ensure(); err != nil {
			return nil, err
		}
		if err := config.WriteFileAtomic(path, []byte(nodes.FileTemplate), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return path, nil
}

// addRouteSettings 补上需要读文件的设置视图：节点文件、实际生效的内网
// 设置和 Tailscale 登录状态。
func (e *Engine) addRouteSettings(view *Settings, settings config.Settings) {
	view.Ready = generator.CheckReady(e.paths.ConfigDir, settings) == nil
	if file, err := nodes.Load(e.proxiesPath(), nil); err != nil {
		view.NodesError = err.Error()
	} else {
		view.NodeCount = file.Count()
	}
	e.mu.Lock()
	profile := e.profile
	e.mu.Unlock()
	if settings.OVPNPath != "" && profile != nil {
		intranet := generator.EffectiveIntranet(profile, settings)
		view.Intranet = IntranetView{
			Domains: nonNil(intranet.Domains), CIDRs: nonNil(intranet.CIDRs), DNS: nonNil(intranet.DNS),
			DomainsAuto: intranet.DomainsAuto, CIDRsAuto: intranet.CIDRsAuto, DNSAuto: intranet.DNSAuto,
		}
	}
	if _, err := os.Stat(filepath.Join(e.paths.ConfigDir, config.TailscaleStateDir)); err == nil {
		view.TailscaleLoggedIn = true
	}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
