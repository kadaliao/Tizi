package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func prepareLinuxDNS(statePath string, resolverPort int, dnsDomains DNSDomains, runner dnsCommandRunner) error {
	if strings.TrimSpace(statePath) == "" {
		return fmt.Errorf("DNS 状态文件路径为空")
	}
	if _, err := os.Stat(statePath); err == nil {
		return fmt.Errorf("检测到未恢复的 DNS 状态，请先执行恢复")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查 DNS 状态: %w", err)
	}
	if err := waitForLinuxTUN(runner, LinuxTUNInterface, 5*time.Second); err != nil {
		return err
	}
	state := DNSState{
		Platform:  "linux",
		Interface: LinuxTUNInterface,
		Service:   "systemd-resolved",
		Automatic: true,
		IPv6:      snapshotLinuxIPv6(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("序列化 DNS 状态: %w", err)
	}
	if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 DNS 状态: %w", err)
	}
	if err := os.Chmod(statePath, 0o600); err != nil {
		_ = os.Remove(statePath)
		return fmt.Errorf("设置 DNS 状态权限: %w", err)
	}

	rollback := func(cause error) error {
		_, _ = runner.PrivilegedOutput("resolvectl", "revert", LinuxTUNInterface)
		if err := restoreLinuxIPv6(runner, state.IPv6); err != nil {
			// 网卡没能还原时保留恢复点，让下次启动或 stop 再试一次。
			return errors.Join(cause, fmt.Errorf("回滚 IPv6: %w", err))
		}
		_ = os.Remove(statePath)
		return cause
	}
	dnsServer := "127.0.0.1:" + strconv.Itoa(resolverPort)
	if output, err := runner.PrivilegedOutput("resolvectl", "dns", LinuxTUNInterface, dnsServer); err != nil {
		return rollback(commandError("设置 Ubuntu TUN DNS", output, err))
	}
	domains := []string{"domain", LinuxTUNInterface, "~."}
	domains = append(domains, linuxSearchDomains(dnsDomains)...)
	if output, err := runner.PrivilegedOutput("resolvectl", domains...); err != nil {
		return rollback(commandError("设置 Ubuntu DNS 路由域", output, err))
	}
	if output, err := runner.PrivilegedOutput("resolvectl", "default-route", LinuxTUNInterface, "true"); err != nil {
		return rollback(commandError("设置 Ubuntu DNS 默认路由", output, err))
	}
	if err := limitLinuxIPv6(runner, state.IPv6); err != nil {
		return rollback(fmt.Errorf("关闭公网 IPv6: %w", err))
	}
	return nil
}

func restoreLinuxDNS(statePath string, runner dnsCommandRunner) (bool, error) {
	if err := checkDNSStateFile(statePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	data, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 DNS 状态: %w", err)
	}
	var state DNSState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, fmt.Errorf("解析 DNS 状态: %w", err)
	}
	if state.Platform != "linux" || state.Interface != LinuxTUNInterface {
		return false, fmt.Errorf("Ubuntu DNS 状态无效")
	}
	output, revertErr := runner.PrivilegedOutput("resolvectl", "revert", state.Interface)
	if revertErr != nil && !linuxLinkMissing(string(output)) {
		return false, commandError("恢复 Ubuntu DNS", output, revertErr)
	}
	if err := cleanupLinuxTUNPolicy(runner); err != nil {
		return false, err
	}
	if err := restoreLinuxIPv6(runner, state.IPv6); err != nil {
		return false, err
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("删除已恢复的 DNS 状态: %w", err)
	}
	return true, nil
}

func activeLinuxDNS(runner dnsCommandRunner) (DNSState, error) {
	routeOutput, err := runner.Output("ip", "-4", "route", "show", "default")
	if err != nil {
		return DNSState{}, commandError("读取默认路由", routeOutput, err)
	}
	iface := linuxDefaultRouteInterface(string(routeOutput))
	if iface == "" {
		return DNSState{}, fmt.Errorf("默认路由缺少网络接口")
	}
	output, err := runner.Output("resolvectl", "dns", iface)
	if err != nil {
		return DNSState{}, commandError("读取 systemd-resolved DNS", output, err)
	}
	servers := parseLinuxDNSServers(string(output))
	return DNSState{
		Platform:  "linux",
		Interface: iface,
		Service:   "systemd-resolved/" + iface,
		Automatic: len(servers) == 0,
		Servers:   servers,
	}, nil
}

func waitForLinuxTUN(runner dnsCommandRunner, iface string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := runner.Output("ip", "link", "show", "dev", iface); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 Ubuntu TUN 接口 %s 超时", iface)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// linuxSearchDomains 返回 TUN 网卡的搜索域（内网域名），让短主机名也能
// 解析。私有域名的查询本来就经 "~." 交给 Mihomo，不需要另设路由域。
func linuxSearchDomains(domains DNSDomains) []string {
	var result []string
	for _, domain := range domains.Search {
		if isManagedResolverDomain(domain) {
			result = append(result, domain)
		}
	}
	return result
}

func linuxDefaultRouteInterface(output string) string {
	fields := strings.Fields(output)
	for index, field := range fields {
		if field == "dev" && index+1 < len(fields) {
			return fields[index+1]
		}
	}
	return ""
}

func parseLinuxDNSServers(output string) []string {
	colon := strings.Index(output, ":")
	if colon < 0 {
		return nil
	}
	var servers []string
	for _, field := range strings.Fields(output[colon+1:]) {
		if field != "" && field != "n/a" {
			servers = append(servers, field)
		}
	}
	return servers
}

func cleanupLinuxTUNPolicy(runner dnsCommandRunner) error {
	output, err := runner.Output("ip", "rule", "show")
	if err != nil {
		return commandError("读取 Ubuntu 策略路由", output, err)
	}
	counts := make(map[int]int)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		colon := strings.IndexByte(line, ':')
		if colon < 1 {
			continue
		}
		priority, parseErr := strconv.Atoi(line[:colon])
		if parseErr != nil || !linuxTUNRulePriority(priority) {
			continue
		}
		if !ownedLinuxTUNRule(priority, line[colon+1:]) {
			return fmt.Errorf("拒绝清理未知的 Ubuntu 策略路由: %s", line)
		}
		counts[priority]++
	}
	if len(counts) == 0 {
		return nil
	}
	for _, priority := range []int{9000, 9001, 9002, 9010} {
		for range counts[priority] {
			output, err := runner.PrivilegedOutput("ip", "rule", "delete", "priority", strconv.Itoa(priority))
			if err != nil {
				return commandError("清理 Ubuntu 策略路由", output, err)
			}
		}
	}
	output, err = runner.PrivilegedOutput("ip", "route", "flush", "table", "2022")
	if err != nil {
		return commandError("清理 Ubuntu TUN 路由表", output, err)
	}
	return nil
}

func linuxTUNRulePriority(priority int) bool {
	return priority == 9000 || priority == 9001 || priority == 9002 || priority == 9010
}

func ownedLinuxTUNRule(priority int, rule string) bool {
	rule = strings.TrimSpace(rule)
	switch priority {
	case 9000:
		return strings.Contains(rule, "to 198.18.0.0/30") && strings.Contains(rule, "lookup 2022")
	case 9001:
		return (strings.Contains(rule, "dport 53") && strings.Contains(rule, "lookup main") && strings.Contains(rule, "suppress_prefixlength 0")) ||
			(strings.Contains(rule, "iif "+LinuxTUNInterface) && strings.Contains(rule, "goto 9010"))
	case 9002:
		return strings.Contains(rule, "iif lo") && strings.Contains(rule, "lookup 2022")
	case 9010:
		return strings.HasSuffix(rule, "nop")
	default:
		return false
	}
}

func linuxLinkMissing(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no such") ||
		strings.Contains(lower, "unknown interface")
}
