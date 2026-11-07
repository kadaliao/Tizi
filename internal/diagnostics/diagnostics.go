package diagnostics

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeblock"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/manager"
	"github.com/kadaliao/tizi/internal/nodes"
	"github.com/kadaliao/tizi/internal/system"
)

type Level string

const (
	OK   Level = "ok"
	Warn Level = "warn"
	Fail Level = "fail"
	Info Level = "info"
)

type Check struct {
	Level  Level
	Name   string
	Detail string
}

func Run(paths config.Paths, settings config.Settings) []Check {
	checks := []Check{{Level: Info, Name: "平台", Detail: runtime.GOOS + "/" + runtime.GOARCH}}
	if supported, detail := system.PlatformSupport(); supported {
		checks = append(checks, Check{Level: OK, Name: "系统支持", Detail: detail})
	} else {
		checks = append(checks, Check{Level: Fail, Name: "系统支持", Detail: detail})
	}

	checks = append(checks, upstreamCheck(paths.ConfigDir, settings))
	profile, profileErr := manager.LoadProfile(settings)
	switch {
	case settings.OVPNPath == "":
		checks = append(checks, Check{Level: Info, Name: "内网 VPN", Detail: "未导入 .ovpn（可选）"})
	case profileErr != nil:
		checks = append(checks, Check{Level: Fail, Name: "内网 VPN", Detail: profileErr.Error()})
	default:
		if selected, err := profile.SelectRemotes(settings.Remote); err != nil {
			checks = append(checks, Check{Level: Fail, Name: "Remote", Detail: err.Error()})
		} else {
			checks = append(checks, Check{Level: OK, Name: "内网 VPN", Detail: fmt.Sprintf("%d 个 remote，当前使用 %d 个", len(profile.Remotes), len(selected))})
		}
		checks = append(checks, intranetChecks(generator.EffectiveIntranet(profile, settings), settings)...)
	}
	if err := config.ValidateTailscale(settings); err != nil {
		checks = append(checks, Check{Level: Fail, Name: "Tailscale", Detail: err.Error()})
	} else if settings.TailscaleEnabled {
		detail := "已开启，设备名 " + generator.TailscaleHostname(settings)
		if settings.TailscaleExitNode != "" {
			detail += "，出口节点 " + settings.TailscaleExitNode
		}
		level := OK
		if _, err := os.Stat(filepath.Join(paths.ConfigDir, config.TailscaleStateDir)); err != nil && settings.TailscaleAuthKey == "" {
			level = Warn
			detail += "；首次登录需要填写 Auth Key"
		}
		checks = append(checks, Check{Level: level, Name: "Tailscale", Detail: detail})
	}

	binary := core.Find(paths)
	if binary == "" {
		checks = append(checks, Check{Level: Warn, Name: "Mihomo", Detail: "未安装；可在状态页选择安装核心"})
	} else {
		checks = append(checks, Check{Level: OK, Name: "Mihomo", Detail: core.Version(binary)})
	}

	managed := (manager.Manager{Paths: paths}).Status()
	vpn := system.InspectVPN()
	if len(vpn.Tunnelblick) > 0 {
		checks = append(checks, Check{Level: Warn, Name: "VPN 冲突", Detail: fmt.Sprintf("检测到 Tunnelblick，%d 个 OpenVPN 进程；启动 TUN 前应先停止", len(vpn.Tunnelblick))})
	} else if len(vpn.OpenVPN) > 0 {
		checks = append(checks, Check{Level: Warn, Name: "VPN 冲突", Detail: fmt.Sprintf("检测到 OpenVPN，%d 个进程；可能与 TUN 路由冲突", len(vpn.OpenVPN))})
	} else {
		checks = append(checks, Check{Level: OK, Name: "VPN 冲突", Detail: "未检测到其他 VPN 进程"})
	}
	if len(vpn.Tailscale) > 0 && settings.TunEnabled {
		detail := "检测到 Tailscale 客户端；连接期间 tailnet 地址（100.64.0.0/10）会被 TUN 接管而无法访问"
		if settings.TailscaleEnabled {
			detail = "检测到 Tailscale 客户端，同时开启了内置 Tailscale；建议只保留一个"
		} else {
			detail += "，可以改用「配置」页的内置 Tailscale"
		}
		checks = append(checks, Check{Level: Warn, Name: "Tailscale 客户端", Detail: detail})
	}
	unmanagedMihomo := 0
	for _, process := range vpn.Mihomo {
		if !managed.Running || !vpn.BelongsToProcessTree(process.PID, managed.PID) {
			unmanagedMihomo++
		}
	}
	if unmanagedMihomo > 0 {
		checks = append(checks, Check{Level: Warn, Name: "其他代理核心", Detail: fmt.Sprintf("检测到 %d 个非受管 Mihomo 进程", unmanagedMihomo)})
	}
	checks = append(checks, Check{Level: Info, Name: "默认路由", Detail: system.DefaultRoute()})
	ipv6Iface, ipv6Err := system.IPv6DefaultRoute()
	checks = append(checks, ipv6Check(ipv6Iface, ipv6Err, settings.TunEnabled && managed.Running))
	if dns, err := system.ActiveDNS(); err != nil {
		checks = append(checks, Check{Level: Warn, Name: "系统 DNS", Detail: err.Error()})
	} else if dns.Automatic {
		checks = append(checks, Check{Level: OK, Name: "系统 DNS", Detail: dns.Service + "：自动获取"})
	} else {
		checks = append(checks, Check{Level: Info, Name: "系统 DNS", Detail: dns.Service + "：" + strings.Join(dns.Servers, ", ")})
	}
	checks = append(checks, Check{Level: Info, Name: "TUN 接口", Detail: joinOr(vpn.UTUNs, "无")})

	ports := []struct {
		name string
		port int
	}{{"Mixed", settings.MixedPort}, {"Controller", settings.ControllerPort}, {"DNS", settings.DNSPort}}
	for _, item := range ports {
		owner := system.PortOwnerProcess(item.port)
		ownedByManaged := managed.Running && vpn.BelongsToProcessTree(owner.PID, managed.PID)
		checks = append(checks, portCheck(item.name, item.port, owner, ownedByManaged))
	}

	if managed.Running {
		checks = append(checks, Check{Level: OK, Name: "受管进程", Detail: fmt.Sprintf("运行中，PID %d", managed.PID)})
	} else {
		checks = append(checks, Check{Level: Info, Name: "受管进程", Detail: "未运行"})
	}
	if _, err := os.Stat(paths.DNSState); err == nil {
		level := Warn
		detail := "检测到待恢复状态；停止或重启 TUI 会自动恢复"
		if managed.Running {
			level = OK
			detail = "已保存启动前状态，停止时自动恢复"
		}
		checks = append(checks, Check{Level: level, Name: "DNS 恢复点", Detail: detail})
	}
	if controller.New(settings.ControllerPort, settings.ControllerSecret).Healthy() {
		checks = append(checks, Check{Level: OK, Name: "Controller API", Detail: fmt.Sprintf("127.0.0.1:%d 可访问", settings.ControllerPort)})
	} else {
		checks = append(checks, Check{Level: Info, Name: "Controller API", Detail: "当前不可访问"})
	}
	if settings.TunEnabled {
		if system.CanUseSudoWithoutPrompt() {
			checks = append(checks, Check{Level: OK, Name: "管理员权限", Detail: "sudo 凭据当前可用"})
		} else {
			checks = append(checks, Check{Level: Info, Name: "管理员权限", Detail: "启动时会自动请求"})
		}
	}
	if profile != nil && managed.Running {
		intranet := generator.EffectiveIntranet(profile, settings)
		if len(intranet.DNS) > 0 {
			target := net.JoinHostPort(intranet.DNS[0], "53")
			if latency, err := system.TCPReachable(target, 1500*time.Millisecond); err != nil {
				checks = append(checks, Check{Level: Warn, Name: "内网连通", Detail: "内网 DNS " + target + " 不可达"})
			} else {
				checks = append(checks, Check{Level: OK, Name: "内网连通", Detail: fmt.Sprintf("内网 DNS %s 可达，%dms", target, latency.Milliseconds())})
			}
		}
	}
	checks = append(checks, residentialCheck(paths.ConfigDir, settings))
	if claudeblock.Supported() {
		checks = append(checks, claudeBlockCheck(claudeblock.Installed(), claudeblock.SelfTest))
	}
	if _, err := os.Stat(paths.Generated); err == nil {
		checks = append(checks, Check{Level: Info, Name: "生成配置", Detail: paths.Generated})
	}
	return checks
}

// residentialCheck 只检查住宅节点的设置，不访问网络；实际线路由
// `tizi claude-check` 和桌面版的 Claude 访问监测检测。
func residentialCheck(configDir string, settings config.Settings) Check {
	const name = "Claude 住宅节点"
	node, err := claude.Load(configDir, settings)
	switch {
	case err != nil:
		return Check{Level: Fail, Name: name, Detail: err.Error()}
	case node == nil:
		detail := "未设置，Claude 经代理节点访问"
		if settings.ClaudeGuard {
			detail += "；访问监测已开启"
		}
		return Check{Level: Info, Name: name, Detail: detail}
	}
	way := "经前置代理链式连接"
	if node.Direct {
		way = "本机直接连接"
	}
	detail := fmt.Sprintf("%s %s:%d，%s", node.Type(), node.Server(), node.Port(), way)
	if hint := node.Hint(); hint != "" {
		return Check{Level: Warn, Name: name, Detail: detail + "。" + hint}
	}
	return Check{Level: OK, Name: name, Detail: detail}
}

// upstreamCheck 报告订阅和节点文件的设置，不访问网络。
func upstreamCheck(configDir string, settings config.Settings) Check {
	const name = "代理节点"
	var parts []string
	if err := config.ValidateSubscription(settings.Subscription); err != nil {
		return Check{Level: Fail, Name: name, Detail: err.Error()}
	}
	if settings.Subscription != "" {
		parts = append(parts, "订阅 "+config.MaskSubscription(settings.Subscription))
	}
	file, err := nodes.Load(filepath.Join(configDir, config.ProxiesFile), nil)
	if err != nil {
		return Check{Level: Fail, Name: name, Detail: err.Error()}
	}
	if count := file.Count(); count > 0 {
		parts = append(parts, fmt.Sprintf("节点文件 %d 个节点", count))
	}
	if settings.OVPNPath != "" && settings.IntranetHTTPProxy != "" {
		parts = append(parts, "内网 HTTP 出口 "+settings.IntranetHTTPProxy)
	}
	if settings.TailscaleEnabled && settings.TailscaleExitNode != "" {
		parts = append(parts, "Tailscale 出口节点 "+settings.TailscaleExitNode)
	}
	if len(parts) == 0 {
		return Check{Level: Warn, Name: name, Detail: "没有订阅或节点文件；外网流量直连，Claude 无法访问"}
	}
	return Check{Level: OK, Name: name, Detail: strings.Join(parts, "；")}
}

// intranetChecks 报告实际生效的内网设置。
func intranetChecks(intranet generator.Intranet, settings config.Settings) []Check {
	if err := config.ValidateIntranet(settings); err != nil {
		return []Check{{Level: Fail, Name: "内网设置", Detail: err.Error()}}
	}
	source := func(auto bool) string {
		if auto {
			return "（来自 .ovpn 或默认值）"
		}
		return ""
	}
	checks := []Check{{Level: Info, Name: "内网网段", Detail: joinOr(intranet.CIDRs, "无") + source(intranet.CIDRsAuto)}}
	if len(intranet.Domains) == 0 {
		checks = append(checks, Check{Level: Info, Name: "内网域名", Detail: "未设置；只有内网网段的 IP 走内网"})
	} else {
		checks = append(checks, Check{Level: Info, Name: "内网域名", Detail: strings.Join(intranet.Domains, ", ") + source(intranet.DomainsAuto)})
	}
	switch {
	case len(intranet.DNS) > 0:
		checks = append(checks, Check{Level: Info, Name: "内网 DNS", Detail: strings.Join(intranet.DNS, ", ") + source(intranet.DNSAuto)})
	case len(intranet.Domains) > 0:
		checks = append(checks, Check{Level: Warn, Name: "内网 DNS", Detail: "设置了内网域名但没有内网 DNS，这些域名会交给公共 DNS 解析，多半解析不了"})
	}
	return checks
}

// ipv6Check 报告系统是否还有绕过 Mihomo TUN 的 IPv6 出口。TUN 只接管 IPv4，
// 连接期间若仍有 IPv6 默认路由，双栈海外站点会从物理网卡直连、暴露真实地址。
// tunActive 为 true 时这是需要处理的问题，否则只是提示。
func ipv6Check(iface string, err error, tunActive bool) Check {
	switch {
	case err != nil:
		return Check{Level: Info, Name: "IPv6 路由", Detail: "无法读取: " + err.Error()}
	case iface == "":
		return Check{Level: OK, Name: "IPv6 路由", Detail: "没有 IPv6 默认路由，海外请求不会经 IPv6 绕过代理"}
	case tunActive:
		return Check{Level: Warn, Name: "IPv6 路由", Detail: fmt.Sprintf("IPv6 默认路由经 %s 直出，双栈海外站点会绕过代理并暴露真实地址；重新连接可再次关闭公网 IPv6", iface)}
	default:
		return Check{Level: Info, Name: "IPv6 路由", Detail: fmt.Sprintf("IPv6 默认路由经 %s；连接后会自动关闭公网 IPv6", iface)}
	}
}

func portCheck(name string, port int, owner system.Process, ownedByManaged bool) Check {
	checkName := fmt.Sprintf("%s 端口 %d", name, port)
	if owner.PID == 0 {
		return Check{Level: OK, Name: checkName, Detail: "可用"}
	}
	ownerName := owner.Command
	if ownerName == "" {
		ownerName = "未知进程"
	}
	detail := fmt.Sprintf("%s (PID %d)", ownerName, owner.PID)
	if ownedByManaged {
		return Check{Level: OK, Name: checkName, Detail: "受管 " + detail}
	}
	return Check{Level: Warn, Name: checkName, Detail: detail}
}

func joinOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return strings.Join(values, ", ")
}

// claudeBlockCheck 报告直连保护是否安装、自检是否通过。
func claudeBlockCheck(installed bool, selfTest func() error) Check {
	const name = "Claude 直连保护"
	if !installed {
		return Check{Level: Info, Name: name, Detail: "未开启；VPN 断开时访问 Claude 会直连"}
	}
	switch err := selfTest(); {
	case err == nil:
		return Check{Level: OK, Name: name, Detail: "已开启，自检通过：不经 VPN 的 Claude 连接会被拦截"}
	case errors.Is(err, claudeblock.ErrSelfTestOffline):
		return Check{Level: Info, Name: name, Detail: "已开启；没有可用的网络，暂时无法自检"}
	default:
		return Check{Level: Fail, Name: name, Detail: "已安装但没有生效：" + err.Error()}
	}
}
