// Package claudeblock 是 Claude 的直连保护：用 macOS 的 pf 防火墙拦截所有不经
// VPN 隧道、直接发往 Anthropic 网段的连接。
//
// 规则只看目标地址和出口网卡，与 VPN 是否在运行无关：VPN 正常时 Claude 的
// 流量进入 Mihomo 的 TUN（utun 网卡）或使用 fake IP，不受影响；Mihomo 崩溃、
// 被结束或还没连接时，发往 Anthropic 的包只能从物理网卡出去，被立即拒绝。
// 一个 LaunchDaemon 在开机时加载规则并定期检查，重启后到连上 VPN 之前也
// 受保护。
//
// 规则挂在系统 /etc/pf.conf 已经引用的 com.apple/* 锚点下，不修改 pf.conf。
package claudeblock

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Networks 是 Anthropic 登记的全部网段（ARIN：Anthropic, PBC）。claude.ai、
// claude.com、api.anthropic.com、claudeusercontent.com 都解析到这里。
var Networks = []string{"160.79.104.0/21", "2607:6bc0::/32"}

// SelfTestAddress 是文档保留地址（TEST-NET-1，RFC 5737），不属于任何真实
// 站点。它和 Anthropic 网段在同一条规则里，向它发起连接就能确认规则在
// 生效，而不必碰 Anthropic 的真实地址。
const SelfTestAddress = "192.0.2.1"

const (
	// Anchor 挂在 /etc/pf.conf 的 anchor "com.apple/*" 下，系统会求值它。
	Anchor = "com.apple/290.Tizi.Claude"
	// RulesPath 是规则文件，root 所有。
	RulesPath = "/etc/pf.anchors/com.kadaliao.tizi.claude"
	// DaemonLabel、DaemonPath 是开机加载规则的 LaunchDaemon。
	DaemonLabel = "com.kadaliao.tizi.claude-block"
	DaemonPath  = "/Library/LaunchDaemons/com.kadaliao.tizi.claude-block.plist"

	// tunnelInterfaces 是放行的 utun 网卡数量。Mihomo 的 TUN 和其他 VPN
	// 都是 utunN，编号由系统分配；更大的编号极少见。
	tunnelInterfaces = 128
	// checkInterval 是 LaunchDaemon 检查规则是否还在的间隔（秒）。
	checkInterval = 60
)

// Supported 报告当前系统是否支持直连保护。
func Supported() bool {
	return runtime.GOOS == "darwin"
}

// Installed 报告直连保护是否已经安装（规则文件和 LaunchDaemon 都在）。
// 两个文件普通用户都能读，不需要管理员权限；规则是否真的在生效用
// SelfTest 确认。
func Installed() bool {
	if !Supported() {
		return false
	}
	for _, path := range []string{RulesPath, DaemonPath} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

// Rules 返回 pf 规则。先放行 utun 网卡，再拒绝其余网卡上发往这些地址的
// 连接；block return 让应用立即失败，而不是等到超时。
func Rules() string {
	tunnels := make([]string, 0, tunnelInterfaces)
	for i := 0; i < tunnelInterfaces; i++ {
		tunnels = append(tunnels, fmt.Sprintf("utun%d", i))
	}
	addresses := append(append([]string(nil), Networks...), SelfTestAddress)
	var builder strings.Builder
	builder.WriteString("# Managed by tizi. Remove with: tizi claude-block off\n")
	builder.WriteString("# Block connections to Anthropic (Claude) that do not go through a VPN tunnel.\n")
	builder.WriteString("# " + SelfTestAddress + " is a documentation address (RFC 5737) used for the self test.\n")
	builder.WriteString("table <tizi_claude> const { " + strings.Join(addresses, ", ") + " }\n")
	builder.WriteString("pass out quick on { " + strings.Join(tunnels, ", ") + " } to <tizi_claude>\n")
	builder.WriteString("block return out quick to <tizi_claude>\n")
	return builder.String()
}

// daemonScript 在规则缺失时重新加载、在 pf 被关闭时重新打开。系统启动时
// 会重新加载 /etc/pf.conf，定期检查保证这之后规则仍在。
func daemonScript() string {
	return fmt.Sprintf(
		"/sbin/pfctl -a '%[1]s' -s rules 2>&1 | /usr/bin/grep -q block || /sbin/pfctl -a '%[1]s' -f '%[2]s'; "+
			"/sbin/pfctl -s info 2>&1 | /usr/bin/grep -q 'Status: Enabled' || /sbin/pfctl -E",
		Anchor, RulesPath)
}

// DaemonPlist 返回 LaunchDaemon 的配置。
func DaemonPlist() string {
	var script bytes.Buffer
	_ = xml.EscapeText(&script, []byte(daemonScript()))
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + DaemonLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>` + script.String() + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StartInterval</key>
	<integer>` + fmt.Sprint(checkInterval) + `</integer>
</dict>
</plist>
`
}

// privileged 以管理员身份运行命令；调用方先完成授权（sudo 凭据已缓存）。
// 测试替换它。
var privileged = func(name string, args ...string) ([]byte, error) {
	if os.Geteuid() == 0 {
		return exec.Command(name, args...).CombinedOutput()
	}
	return exec.Command("sudo", append([]string{"-n", name}, args...)...).CombinedOutput()
}

// Install 安装或更新直连保护并立即生效。调用前需要已经取得管理员授权。
func Install() error {
	if !Supported() {
		return errUnsupported
	}
	if err := installFile(RulesPath, Rules()); err != nil {
		return err
	}
	if err := installFile(DaemonPath, DaemonPlist()); err != nil {
		return err
	}
	if output, err := privileged("/sbin/pfctl", "-a", Anchor, "-f", RulesPath); err != nil {
		return commandError("加载防火墙规则", output, err)
	}
	info, err := privileged("/sbin/pfctl", "-s", "info")
	if err != nil {
		return commandError("读取防火墙状态", info, err)
	}
	if !strings.Contains(string(info), "Status: Enabled") {
		// -E 按引用计数打开 pf，不影响其他也在用 pf 的程序。
		if output, err := privileged("/sbin/pfctl", "-E"); err != nil {
			return commandError("打开防火墙", output, err)
		}
	}
	// 已经加载过时先卸载，让新的配置生效；没加载时卸载失败是正常的。
	_, _ = privileged("/bin/launchctl", "bootout", "system/"+DaemonLabel)
	if output, err := privileged("/bin/launchctl", "bootstrap", "system", DaemonPath); err != nil {
		return commandError("注册开机加载", output, err)
	}
	rules, err := privileged("/sbin/pfctl", "-a", Anchor, "-s", "rules")
	if err != nil || !strings.Contains(string(rules), "block return") {
		return commandError("确认防火墙规则", rules, err)
	}
	return nil
}

// Remove 卸载直连保护。pf 保持打开：其他程序可能也在用它，只剩系统默认
// 规则时打开 pf 没有影响，重启后恢复系统默认状态。
func Remove() error {
	if !Supported() {
		return errUnsupported
	}
	_, _ = privileged("/bin/launchctl", "bootout", "system/"+DaemonLabel)
	if output, err := privileged("/bin/rm", "-f", DaemonPath, RulesPath); err != nil {
		return commandError("删除直连保护文件", output, err)
	}
	if output, err := privileged("/sbin/pfctl", "-a", Anchor, "-F", "all"); err != nil {
		return commandError("清除防火墙规则", output, err)
	}
	return nil
}

var errUnsupported = errors.New("直连保护目前只支持 macOS")

// installFile 以 root 所有、0644 权限写入 path：先写到当前用户的临时文件，
// 再用 install 复制过去。
func installFile(path, content string) error {
	temp, err := os.CreateTemp("", "tizi-claude-block-*")
	if err != nil {
		return fmt.Errorf("准备 %s: %w", path, err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.WriteString(content); err != nil {
		_ = temp.Close()
		return fmt.Errorf("准备 %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("准备 %s: %w", path, err)
	}
	if output, err := privileged("/usr/bin/install", "-m", "0644", "-o", "root", "-g", "wheel", temp.Name(), path); err != nil {
		return commandError("写入 "+path, output, err)
	}
	return nil
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	switch {
	case err != nil && detail != "":
		return fmt.Errorf("%s失败: %s", action, detail)
	case err != nil:
		return fmt.Errorf("%s失败: %w", action, err)
	case detail != "":
		return fmt.Errorf("%s失败: %s", action, detail)
	default:
		return fmt.Errorf("%s失败", action)
	}
}
