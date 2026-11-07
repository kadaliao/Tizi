package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/kadaliao/tizi/internal/appupdate"
	"github.com/kadaliao/tizi/internal/claudeblock"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/configshare"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/customrules"
	"github.com/kadaliao/tizi/internal/diagnostics"
	"github.com/kadaliao/tizi/internal/engine"
	"github.com/kadaliao/tizi/internal/manager"
	"github.com/kadaliao/tizi/internal/system"
	"github.com/kadaliao/tizi/internal/tracescope"
	"github.com/kadaliao/tizi/internal/ui"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run 执行命令行入口；-h/--help 打印用法后视为成功退出。
func run(args []string) error {
	err := runCommand(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runCommand(args []string) error {
	if len(args) > 0 {
		// 帮助与版本不依赖配置目录和设置文件，放在最前面，避免损坏的
		// settings.json 或误用 sudo 时连版本号都看不到。
		switch args[0] {
		case "-h", "--help", "help":
			usage()
			return nil
		case "-v", "--version", "version":
			fmt.Println("tizi", version)
			return nil
		}
	}
	if len(args) > 0 && args[0] == "_run-logged-core" {
		if len(args) != 6 {
			return fmt.Errorf("内部日志启动参数无效")
		}
		useSudo, err := strconv.ParseBool(args[5])
		if err != nil {
			return fmt.Errorf("解析内部启动参数: %w", err)
		}
		return manager.RunLoggedCore(args[1], args[2], args[3], args[4], useSudo)
	}
	if len(args) > 0 && args[0] == "_recover-dns-after-exit" {
		if len(args) != 4 {
			return fmt.Errorf("内部 DNS 恢复参数无效")
		}
		parentPID, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("解析内部 DNS 恢复参数: %w", err)
		}
		return manager.RunDNSRecoveryGuard(args[1], parentPID, args[3])
	}
	if os.Geteuid() == 0 {
		if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); sudoUser != "" && sudoUser != "root" {
			return fmt.Errorf("不要使用 sudo 运行 tizi；请退出并以用户 %s 直接运行，工具会在需要时请求管理员权限", sudoUser)
		}
		return fmt.Errorf("不要以 root 身份运行 tizi；请使用普通用户直接运行，工具会在需要时请求管理员权限")
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "init" {
		if len(args) != 1 {
			return fmt.Errorf("init 不接受参数")
		}
		if err := initialize(paths); err != nil {
			return err
		}
		fmt.Println("已初始化用户配置:", paths.CustomRules)
		return nil
	}
	settings, err := config.Load(paths)
	if err != nil {
		return err
	}
	needsSecret := len(args) == 0
	if len(args) > 0 {
		switch args[0] {
		case "tui", "generate", "validate", "start", "reload", "clash-qr", "dashboard", "_gui-engine":
			needsSecret = true
		}
	}
	if needsSecret {
		if err := config.EnsureControllerSecret(paths, &settings); err != nil {
			return err
		}
	}
	man := manager.Manager{Paths: paths}
	if len(args) == 0 || args[0] == "tui" {
		if len(args) > 1 {
			return fmt.Errorf("tui 不接受参数: %s", strings.Join(args[1:], " "))
		}
		if err := initialize(paths); err != nil {
			return err
		}
		if _, err := man.RecoverDNS(); err != nil {
			return fmt.Errorf("恢复上次运行遗留的 DNS: %w", err)
		}
		program := tea.NewProgram(ui.New(paths, settings, version))
		finalModel, runErr := program.Run()
		if errors.Is(runErr, tea.ErrInterrupted) {
			// SIGINT/SIGTERM 触发的退出是用户主动行为，照常清理后正常退出，
			// 不再打印 "error: interrupted"。
			runErr = nil
		}
		if m, ok := finalModel.(ui.Model); ok && m.RestartAfterUpdate() && runErr == nil {
			return restartUpdatedTUI(man, args)
		}
		if m, ok := finalModel.(ui.Model); ok && m.KeepRunning() {
			// UI 快照最多滞后两秒，跳过清理前必须确认进程真的还在，
			// 否则会留下 PID 文件和被接管的系统 DNS。
			if man.Status().Running {
				fmt.Println("Mihomo 保持运行；需要时执行 tizi stop 停止并恢复 DNS")
				return runErr
			}
			fmt.Println("受管 Mihomo 已退出，执行清理并恢复系统 DNS")
		}
		stopErr := man.Stop()
		return errors.Join(runErr, stopErr)
	}

	switch args[0] {
	case "_gui-engine":
		return runGUIEngine(paths, settings, args[1:])
	case "update":
		if len(args) != 1 {
			return fmt.Errorf("update 不接受参数")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		status, err := appupdate.Check(ctx, version)
		if err != nil {
			return err
		}
		if !status.Available {
			fmt.Printf("当前已是最新版本 %s\n", status.Current)
			return nil
		}
		fmt.Fprintf(os.Stderr, "发现新版本 %s，正在更新...\n", status.Latest)
		result, err := appupdate.Install(ctx, status.Latest)
		if err != nil {
			return err
		}
		if note := result.DesktopNote(); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
		// 受管 Mihomo 正在运行时，用新二进制重载让它立即生效；未运行时
		// 保持不启动，避免更新动作顺带拉起 VPN。
		if !man.Status().Running {
			fmt.Printf("APP 已更新到 %s；下次启动生效\n", status.Latest)
			return nil
		}
		executable, err := appupdate.ExecutablePath()
		if err != nil {
			return fmt.Errorf("APP 已更新到 %s，但定位新版本失败: %w；请手动执行 tizi reload", status.Latest, err)
		}
		if err := reloadWithBinary(executable); err != nil {
			return fmt.Errorf("APP 已更新到 %s，但新版本重载 Mihomo 失败（%w）；处理上方提示后执行 tizi reload", status.Latest, err)
		}
		fmt.Printf("APP 已更新到 %s，Mihomo 已用新版本重载生效\n", status.Latest)
		return nil
	case "diagnose":
		for _, check := range diagnostics.Run(paths, settings) {
			fmt.Printf("%-5s  %-20s %s\n", strings.ToUpper(string(check.Level)), check.Name, check.Detail)
		}
		return nil
	case "recover-wechat":
		if len(args) != 1 {
			return fmt.Errorf("recover-wechat 不接受参数")
		}
		return runRecoverWeChat(man.RecoverWeChatConnections, os.Stdout)
	case "generate", "validate", "start", "reload", "clash-qr":
		updated, force, err := parseConnectionFlags(args[0], args[1:], settings)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(updated, settings) {
			if err := config.Save(paths, updated); err != nil {
				return err
			}
		}
		switch args[0] {
		case "clash-qr":
			return runClashQRCode(man, updated)
		case "generate":
			profile, err := man.Generate(updated)
			if err != nil {
				return err
			}
			if profile != nil {
				fmt.Printf("已生成 %s（内网 %d 个 remote）\n", paths.Generated, len(profile.Remotes))
			} else {
				fmt.Printf("已生成 %s\n", paths.Generated)
			}
			return nil
		case "validate":
			if err := man.Validate(updated); err != nil {
				return err
			}
			fmt.Println("配置有效:", paths.Generated)
			return nil
		case "start":
			updated.AllowVPNConflict = force
			pid, err := man.Start(updated)
			if errors.Is(err, manager.ErrVPNConflict) {
				return fmt.Errorf("%w；请先停止其他 VPN 客户端，或明确传入 --force-vpn-conflict", err)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Mihomo 已启动，PID %d，日志 %s\n", pid, paths.LogFile)
			printProxyAuth(updated)
			return nil
		case "reload":
			updated.AllowVPNConflict = force
			pid, err := man.Reload(updated)
			if errors.Is(err, manager.ErrVPNConflict) {
				return fmt.Errorf("%w；请先停止其他 VPN 客户端，或明确传入 --force-vpn-conflict", err)
			}
			if err != nil {
				return err
			}
			fmt.Printf("已重新加载自定义规则，Mihomo PID %d，日志 %s\n", pid, paths.LogFile)
			printProxyAuth(updated)
			return nil
		}
	case "stop":
		if len(args) != 1 {
			return fmt.Errorf("stop 不接受参数")
		}
		if err := man.Stop(); err != nil {
			return err
		}
		fmt.Println("受管 Mihomo 已停止，系统 DNS 已恢复")
		return nil
	case "claude-check":
		if len(args) != 1 {
			return fmt.Errorf("claude-check 不接受参数")
		}
		return runClaudeCheck(man, settings, os.Stdout)
	case "claude-block":
		if len(args) != 2 {
			return fmt.Errorf("用法: tizi claude-block on|off|status")
		}
		return runClaudeBlock(args[1], os.Stdout)
	case "status":
		status := man.Status()
		if status.Running {
			fmt.Printf("running pid=%d core=%s\n", status.PID, status.CoreVersion)
		} else {
			fmt.Printf("stopped core=%s\n", status.CoreVersion)
		}
		return nil
	case "install-core":
		flags := flag.NewFlagSet("install-core", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		channelValue := flags.String("channel", string(settings.CoreChannel), "Mihomo 核心通道：stable 或 alpha")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("install-core 不支持的位置参数: %s", strings.Join(flags.Args(), " "))
		}
		channel, err := config.ParseCoreChannel(strings.ToLower(strings.TrimSpace(*channelValue)))
		if err != nil {
			return err
		}
		lastPhase := core.Phase(-1)
		lastPercent := -1
		tag, err := core.Install(context.Background(), paths, channel, func(phase core.Phase, downloaded, total int64) {
			if phase != lastPhase {
				lastPhase, lastPercent = phase, -1
				switch phase {
				case core.PhaseResolve:
					fmt.Fprintln(os.Stderr, "查询 Mihomo 最新版本...")
				case core.PhaseCore:
					fmt.Fprintln(os.Stderr, "下载 Mihomo 核心...")
				case core.PhaseGeoIP:
					fmt.Fprintln(os.Stderr, "下载 GeoIP 数据...")
				}
			}
			if total <= 0 {
				return
			}
			percent := int(downloaded * 100 / total)
			if percent/10 > lastPercent/10 {
				lastPercent = percent
				fmt.Fprintf(os.Stderr, "  %d%% (%.1f/%.1f MiB)\n", percent, float64(downloaded)/(1<<20), float64(total)/(1<<20))
			}
		})
		if err != nil {
			return err
		}
		fmt.Printf("Mihomo %s 已安装到 %s\n", tag, paths.CoreBin)
		return nil
	case "dashboard":
		if len(args) != 1 {
			return fmt.Errorf("dashboard 不接受参数")
		}
		url := controller.DashboardURL(settings.ControllerPort, settings.ControllerSecret)
		if err := exec.Command("open", url).Run(); err != nil {
			return err
		}
		fmt.Println(url)
		return nil
	case "tracescope":
		flags := flag.NewFlagSet("tracescope", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		port := flags.Int("port", 0, "看板页面端口（默认 8788，可用 TRACESCOPE_PORT 环境变量覆盖）")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("tracescope 不支持的位置参数: %s", strings.Join(flags.Args(), " "))
		}
		portValue := *port
		if portValue == 0 {
			if envValue := strings.TrimSpace(os.Getenv("TRACESCOPE_PORT")); envValue != "" {
				parsed, err := strconv.Atoi(envValue)
				if err != nil || parsed < 1 || parsed > 65535 {
					return fmt.Errorf("TRACESCOPE_PORT 无效: %q", envValue)
				}
				portValue = parsed
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return tracescope.Run(ctx, tracescope.Options{
			ConfigPath: paths.Generated,
			Port:       portValue,
			Open:       true,
			VPNActive:  man.Status().Running,
			Stdout:     os.Stdout,
			Stderr:     os.Stderr,
		})
	default:
		usage()
		return fmt.Errorf("未知命令 %q", args[0])
	}
	return nil
}

// runGUIEngine 是 macOS 图形界面启动的后台引擎入口。首次进入时先把自己放进
// 带伪终端的新会话再重新执行，让 sudo 凭据能在整个进程树内复用；会话内的
// 进程通过标准输入输出与 GUI 交换 JSON 行。
func runGUIEngine(paths config.Paths, settings config.Settings, args []string) error {
	flags := flag.NewFlagSet("_gui-engine", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	askpass := flags.String("askpass", "", "显示管理员密码框的程序（绝对路径，必填）")
	inSession := flags.Bool("in-session", false, "内部参数：已在伪终端会话中")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("_gui-engine 不支持的位置参数: %s", strings.Join(flags.Args(), " "))
	}
	// 引擎的伪终端没人能输入，不能退回到终端里读密码，否则授权会一直挂起。
	if !filepath.IsAbs(*askpass) {
		return fmt.Errorf("--askpass 必须是显示密码框程序的绝对路径")
	}
	if !*inSession {
		return engine.RunInTerminalSession(append([]string{"_gui-engine", "--in-session"}, args...))
	}
	engine.PrepareSessionChild()
	system.UseAskPass(*askpass)
	extendGUIPath()
	if err := initialize(paths); err != nil {
		return err
	}
	// GUI 异常退出后继续写标准输出时返回 EPIPE，而不是被 SIGPIPE 直接结束，
	// 让进行中的启动或停止操作完整收尾。
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return engine.New(paths, settings, version, os.Stdout).Serve(ctx, os.Stdin)
}

// extendGUIPath 补上从 Finder 启动时缺少的常用目录。GUI 继承的 PATH 只有
// 系统目录，而 TUI 在 shell 里能通过 Homebrew 或 ~/.local/bin 找到 Mihomo。
func extendGUIPath() {
	current := filepath.SplitList(os.Getenv("PATH"))
	seen := make(map[string]bool, len(current))
	for _, dir := range current {
		seen[dir] = true
	}
	extra := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		extra = append(extra, filepath.Join(home, ".local", "bin"))
	}
	for _, dir := range extra {
		if !seen[dir] {
			current = append(current, dir)
		}
	}
	_ = os.Setenv("PATH", strings.Join(current, string(os.PathListSeparator)))
}

func initialize(paths config.Paths) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	return customrules.EnsureTemplate(paths.CustomRules)
}

func runClashQRCode(man manager.Manager, settings config.Settings) error {
	shareConfig, err := man.GenerateShare(settings)
	if err != nil {
		return err
	}
	session, err := configshare.Start(shareConfig)
	if err != nil {
		return err
	}
	defer session.Close()
	qrCode, _, err := configshare.RenderANSIQRCode(session.URL)
	if err != nil {
		return err
	}
	fmt.Println("Clash Mi 扫码导入（我的配置 → 右上角 + → 扫描二维码）")
	fmt.Println("配置含订阅地址、节点密码或个人证书，请勿截图或转发。")
	fmt.Println(qrCode)
	fmt.Printf("配置地址: %s\n", session.URL)
	fmt.Printf("有效期至: %s；导入完成后按 Ctrl+C 立即关闭。\n", session.ExpiresAt.Format("15:04:05"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-session.Done():
		fmt.Println("配置分享已自动关闭")
		return nil
	case <-ctx.Done():
		if err := session.Close(); err != nil {
			return err
		}
		fmt.Println("\n配置分享已关闭")
		return nil
	}
}

func parseConnectionFlags(command string, args []string, settings config.Settings) (config.Settings, bool, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	subscription := flags.String("subscription", settings.Subscription, "机场订阅地址（传空字符串清除）")
	ovpnPath := flags.String("ovpn", settings.OVPNPath, "内网 VPN 的 .ovpn 文件路径（传空字符串移除）")
	intranetDomains := flags.String("intranet-domains", config.JoinList(settings.IntranetDomains), "走内网的域名后缀，逗号分隔")
	intranetCIDRs := flags.String("intranet-cidrs", config.JoinList(settings.IntranetCIDRs), "走内网的 IP 网段，逗号分隔")
	intranetDNS := flags.String("intranet-dns", config.JoinList(settings.IntranetDNS), "内网 DNS 服务器，逗号分隔")
	intranetHTTP := flags.String("intranet-http", settings.IntranetHTTPProxy, "内网 HTTP 出口 host:port")
	tailscale := flags.Bool("tailscale", settings.TailscaleEnabled, "开启内置 Tailscale")
	noTailscale := flags.Bool("no-tailscale", false, "关闭内置 Tailscale")
	tailscaleKey := flags.String("tailscale-auth-key", settings.TailscaleAuthKey, "Tailscale Auth Key（首次登录需要）")
	tailscaleExit := flags.String("tailscale-exit-node", settings.TailscaleExitNode, "Tailscale 出口节点（名字或 IP）")
	remote := flags.String("remote", settings.Remote, "remote host、host:port、短名称或逗号分隔列表")
	tun := flags.Bool("tun", false, "开启 TUN 并接管系统流量")
	noTun := flags.Bool("no-tun", false, "关闭 TUN，仅提供本地代理端口")
	allowLAN := flags.Bool("allow-lan", settings.AllowLAN, "允许局域网设备连接 Mixed 代理端口")
	noAllowLAN := flags.Bool("no-allow-lan", false, "仅允许本机连接 Mixed 代理端口")
	proxyUser := flags.String("proxy-user", settings.ProxyUsername, "局域网代理认证的用户名")
	proxyAuth := flags.Bool("proxy-auth", false, "局域网连接 Mixed 代理端口需要用户名和密码，没有密码时自动生成")
	noProxyAuth := flags.Bool("no-proxy-auth", false, "局域网连接 Mixed 代理端口不需要认证，清除用户名和密码")
	directChina := flags.Bool("direct-china", settings.ChinaDirect, "国内流量直连")
	proxyChina := flags.Bool("proxy-china", false, "国内流量也走外网代理")
	force := flags.Bool("force-vpn-conflict", false, "其他 OpenVPN 进程活跃时仍尝试启动")
	if err := flags.Parse(args); err != nil {
		return settings, false, err
	}
	if flags.NArg() != 0 {
		return settings, false, fmt.Errorf("不支持的位置参数: %s", strings.Join(flags.Args(), " "))
	}
	normalizedOVPN, err := normalizeOVPNPath(*ovpnPath)
	if err != nil {
		return settings, false, err
	}
	if normalizedOVPN != settings.OVPNPath && *remote == settings.Remote {
		settings.Remote = ""
	} else {
		settings.Remote = strings.TrimSpace(*remote)
	}
	settings.OVPNPath = normalizedOVPN
	settings.Subscription = strings.TrimSpace(*subscription)
	if err := config.ValidateSubscription(settings.Subscription); err != nil {
		return settings, false, err
	}
	settings.IntranetDomains = config.NormalizeDomains(config.SplitList(*intranetDomains))
	settings.IntranetCIDRs = config.SplitList(*intranetCIDRs)
	settings.IntranetDNS = config.SplitList(*intranetDNS)
	settings.IntranetHTTPProxy = strings.TrimSpace(*intranetHTTP)
	if err := config.ValidateIntranet(settings); err != nil {
		return settings, false, err
	}
	if *tailscale && *noTailscale {
		return settings, false, fmt.Errorf("--tailscale 和 --no-tailscale 不能同时使用")
	}
	settings.TailscaleEnabled = *tailscale && !*noTailscale
	settings.TailscaleAuthKey = strings.TrimSpace(*tailscaleKey)
	settings.TailscaleExitNode = strings.TrimSpace(*tailscaleExit)
	if err := config.ValidateTailscale(settings); err != nil {
		return settings, false, err
	}
	if *tun && *noTun {
		return settings, false, fmt.Errorf("--tun 和 --no-tun 不能同时使用")
	}
	if *tun {
		settings.TunEnabled = true
	} else if *noTun {
		settings.TunEnabled = false
	}
	if *noAllowLAN {
		settings.AllowLAN = false
	} else {
		settings.AllowLAN = *allowLAN
	}
	if *proxyAuth && *noProxyAuth {
		return settings, false, fmt.Errorf("--proxy-auth 和 --no-proxy-auth 不能同时使用")
	}
	settings.ProxyUsername = strings.TrimSpace(*proxyUser)
	if *proxyAuth {
		if settings.ProxyUsername == "" {
			return settings, false, fmt.Errorf("--proxy-auth 需要先用 --proxy-user 设置用户名")
		}
		if settings.ProxyPassword == "" {
			password, err := config.NewProxyPassword()
			if err != nil {
				return settings, false, err
			}
			settings.ProxyPassword = password
		}
	} else if *noProxyAuth {
		settings.ProxyUsername = ""
		settings.ProxyPassword = ""
	}
	if settings.ProxyAuthEnabled() {
		if err := config.ValidateProxyCredentials(settings.ProxyUsername, settings.ProxyPassword); err != nil {
			return settings, false, err
		}
	}
	if *proxyChina {
		settings.ChinaDirect = false
	} else {
		settings.ChinaDirect = *directChina
	}
	return settings, *force, nil
}

// printProxyAuth 在局域网代理要求认证时打印凭据，其他设备配置代理时要用。
func printProxyAuth(settings config.Settings) {
	if !settings.AllowLAN || !settings.ProxyAuthEnabled() {
		return
	}
	fmt.Printf("局域网代理需要认证: 端口 %d，用户名 %s，密码 %s\n", settings.MixedPort, settings.ProxyUsername, settings.ProxyPassword)
}

// normalizeOVPNPath 把命令行传入的 OVPN 路径展开 ~ 并转成绝对路径后再持久化，
// 避免相对路径写入 settings.json 后换个工作目录就找不到文件。空值保持为空。
func normalizeOVPNPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("展开 OVPN 路径: %w", err)
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析 OVPN 路径: %w", err)
	}
	return filepath.Clean(abs), nil
}

// restartUpdatedTUI 在 APP 更新后把界面交给新二进制：当前进程仍是旧代码，
// 继续在这里重载或生成配置会沿用旧规则。运行中的 Mihomo 先由新版本重载，
// 再用 exec 原地换成新版本界面。
func restartUpdatedTUI(man manager.Manager, args []string) error {
	executable, err := appupdate.ExecutablePath()
	if err != nil {
		return err
	}
	if man.Status().Running {
		fmt.Println("正在用新版本重载 Mihomo...")
		if err := reloadWithBinary(executable); err != nil {
			return fmt.Errorf("新版本重载 Mihomo 失败（%w）；处理上方提示后执行 tizi reload", err)
		}
	}
	return syscall.Exec(executable, append([]string{executable}, args...), os.Environ())
}

// reloadWithBinary 用指定二进制执行 reload，并把终端交给它，以便需要时
// 由新版本自己请求管理员密码并输出结果。
func reloadWithBinary(executable string) error {
	cmd := exec.Command(executable, "reload")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runClaudeCheck 检测一次 Claude 的线路并打印结论。没有开启监测时也检测，
// 方便排查；有问题时返回错误，脚本可以按退出码判断。
func runClaudeCheck(man manager.Manager, settings config.Settings, output io.Writer) error {
	env := man.ClaudeEnv(settings)
	enabled := env.Enabled
	env.Enabled = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status := claudeguard.Observe(ctx, env)
	fmt.Fprintln(output, status.Title)
	if status.Detail != "" {
		fmt.Fprintln(output, status.Detail)
	}
	if status.Route != "" {
		fmt.Fprintln(output, "线路:", status.Route)
	}
	if status.ExitIP != "" && status.Level != claudeguard.LevelOK {
		fmt.Fprintf(output, "出口: %s %s\n", status.ExitIP, status.Country)
	}
	if !enabled {
		fmt.Fprintln(output, "（未开启 Claude 访问监测：配置住宅节点，或在配置页打开「Claude 访问监测」后，桌面版会在出问题时弹窗）")
	}
	if status.Level == claudeguard.LevelAlert {
		return fmt.Errorf("Claude 线路异常")
	}
	return nil
}

// runClaudeBlock 安装、卸载直连保护或查看它是否生效。
func runClaudeBlock(action string, output io.Writer) error {
	if !claudeblock.Supported() {
		return fmt.Errorf("直连保护目前只支持 macOS")
	}
	switch action {
	case "on", "off":
		if err := system.AuthorizeAdministrator(); err != nil {
			return err
		}
		if action == "off" {
			if err := claudeblock.Remove(); err != nil {
				return err
			}
			fmt.Fprintln(output, "直连保护已关闭：VPN 断开时访问 Claude 会直连")
			return nil
		}
		if err := claudeblock.Install(); err != nil {
			return err
		}
		fmt.Fprintln(output, "直连保护已开启：不经 VPN 的 Claude 连接会被拦截，重启后依然有效")
		return reportClaudeBlockTest(output)
	case "status":
		if !claudeblock.Installed() {
			fmt.Fprintln(output, "直连保护未开启；开启：tizi claude-block on")
			return nil
		}
		fmt.Fprintln(output, "直连保护已安装")
		return reportClaudeBlockTest(output)
	default:
		return fmt.Errorf("用法: tizi claude-block on|off|status")
	}
}

func reportClaudeBlockTest(output io.Writer) error {
	err := claudeblock.SelfTest()
	switch {
	case err == nil:
		fmt.Fprintf(output, "自检通过：从物理网卡直连被拦截的地址（%s）立即被拒绝\n", claudeblock.SelfTestAddress)
		return nil
	case errors.Is(err, claudeblock.ErrSelfTestOffline):
		fmt.Fprintln(output, "没有可用的网络，暂时无法自检")
		return nil
	default:
		return fmt.Errorf("自检没有通过：%w", err)
	}
}

func runRecoverWeChat(recover func() (int, error), output io.Writer) error {
	closed, err := recover()
	if err != nil {
		return fmt.Errorf("恢复微信图片连接（已刷新 %d 条）: %w", closed, err)
	}
	_, err = fmt.Fprintf(output, "已刷新 %d 条微信直连，Mihomo 保持运行\n", closed)
	return err
}

func usage() {
	fmt.Print(`tizi - 基于 Mihomo 的代理工具：订阅分流、可选的内网 OpenVPN 和 Tailscale

用法:
  tizi                        启动交互式 TUI
  tizi init                   初始化用户配置和自定义规则模板
  tizi diagnose               只读诊断本机网络和依赖
  tizi generate [选项]        生成 Mihomo YAML
  tizi clash-qr [选项]        生成配置并显示 Clash Mi 导入二维码
  tizi validate [选项]        生成并调用 Mihomo 校验
  tizi start [选项]           启动受管 Mihomo
  tizi reload [选项]          重新生成配置并重启 Mihomo（应用自定义规则）
  tizi recover-wechat         无中断刷新微信图片直连
  tizi stop                   仅停止本工具启动的 Mihomo
  tizi status                 输出受管进程状态
  tizi claude-check           检测 Claude 的线路和出口 IP
  tizi claude-block on|off|status
                              直连保护：用防火墙拦截不经隧道的 Claude 连接（macOS）
  tizi update                 检查并更新 APP
  tizi install-core [选项]    下载并校验官方 Mihomo 核心
  tizi dashboard              打开 Mihomo Dashboard
  tizi tracescope [选项]      启动本机 TraceScope 3D 网络可视化看板

核心安装选项:
  --channel stable|alpha             安装稳定版或滚动 Alpha 构建

看板选项:
  --port N                           看板页面端口（默认 8788）

连接选项（会保存到设置）:
  --subscription URL                 机场订阅地址（Clash/Mihomo 或 Base64 分享链接）
  --ovpn PATH                        内网 VPN 的 .ovpn 文件（可选；传 "" 移除）
  --remote HOST[,HOST...]            仅使用指定 remote，可用逗号选择多个
  --intranet-domains LIST            走内网的域名后缀，逗号分隔（默认取 .ovpn 的 dhcp-option）
  --intranet-cidrs LIST              走内网的网段，逗号分隔（默认取 .ovpn 的 route，没有时 10.0.0.0/8）
  --intranet-dns LIST                内网 DNS，逗号分隔（默认取 .ovpn 的 dhcp-option DNS）
  --intranet-http HOST:PORT          内网 HTTP 出口，可在「外网代理」里选用
  --tailscale / --no-tailscale       开启或关闭内置 Tailscale
  --tailscale-auth-key KEY           Tailscale Auth Key（首次登录需要）
  --tailscale-exit-node NODE         经 Tailscale 出口节点上网（作为「外网代理」的一个选项）
  --tun                              开启 TUN 并接管系统流量
  --no-tun                           不接管系统流量
  --allow-lan                        允许局域网设备连接 Mixed 代理端口
  --no-allow-lan                     仅允许本机连接 Mixed 代理端口
  --proxy-user NAME                  局域网代理认证的用户名
  --proxy-auth                       局域网连接需要用户名和密码（没有密码时自动生成）
  --no-proxy-auth                    局域网连接不需要认证，清除用户名和密码
  --direct-china                     国内流量直连
  --proxy-china                      国内流量也走外网代理
  --force-vpn-conflict               明确允许与其他 OpenVPN 并行尝试
`)
}
