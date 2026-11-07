package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/kadaliao/tizi/internal/config"
)

type DNSState struct {
	Platform              string              `json:"platform,omitempty"`
	Interface             string              `json:"interface,omitempty"`
	Service               string              `json:"service"`
	Automatic             bool                `json:"automatic"`
	Servers               []string            `json:"servers,omitempty"`
	SearchDomains         []string            `json:"search_domains,omitempty"`
	SearchDomainsCaptured bool                `json:"search_domains_captured,omitempty"`
	Resolvers             []ResolverFileState `json:"resolvers,omitempty"`
	// IPv6 是被限制过 IPv6 的网络服务（macOS）或网卡（Linux）在限制之前的配置。
	IPv6 []IPv6ServiceState `json:"ipv6,omitempty"`
}

type ResolverFileState struct {
	Domain  string `json:"domain"`
	Existed bool   `json:"existed"`
	Content string `json:"content,omitempty"`
}

type dnsCommandRunner interface {
	Output(name string, args ...string) ([]byte, error)
	PrivilegedOutput(name string, args ...string) ([]byte, error)
	Run(name string, args ...string) error
}

type execDNSRunner struct{}

var resolverDir = "/etc/resolver"

// DNSDomains 是连接期间交给 Mihomo DNS 的私有域名。
type DNSDomains struct {
	// Private 是内网域名和 MagicDNS（ts.net）：macOS 上为它们写
	// /etc/resolver 文件，Ubuntu 上设为 TUN 网卡的路由域。
	Private []string
	// Search 是加入系统搜索域的内网域名，让短主机名也能解析。
	Search []string
}

// resolverDomains 返回要写 /etc/resolver 文件的域名：私有域名加上固定交给
// Mihomo 解析的外网域名族，去重且只保留合法的文件名。
func resolverDomains(private []string) []string {
	var result []string
	seen := map[string]bool{}
	for _, domain := range append(append([]string(nil), private...), config.ExternalManagedResolverDomains...) {
		if seen[domain] || !isManagedResolverDomain(domain) {
			continue
		}
		seen[domain] = true
		result = append(result, domain)
	}
	return result
}

func (execDNSRunner) Output(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func (execDNSRunner) PrivilegedOutput(name string, args ...string) ([]byte, error) {
	if os.Geteuid() == 0 {
		return exec.Command(name, args...).CombinedOutput()
	}
	return exec.Command("sudo", append([]string{"-n", name}, args...)...).CombinedOutput()
}

func (execDNSRunner) Run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// ActiveDNS returns the DNS configuration for the network service carrying the
// default route. Automatic means the service inherits DNS from DHCP or RA.
func ActiveDNS() (DNSState, error) {
	switch runtime.GOOS {
	case "darwin":
		return activeDNS(execDNSRunner{})
	case "linux":
		return activeLinuxDNS(execDNSRunner{})
	default:
		return DNSState{}, fmt.Errorf("当前平台不支持 DNS 管理")
	}
}

// PrepareDNS snapshots the current DNS state and resets manual overrides before
// Mihomo creates its TUN interface. It also limits the physical network
// services to link-local IPv6 (Ubuntu: disables IPv6 on physical interfaces),
// because the TUN only captures IPv4 and public IPv6 would bypass the proxy.
// The snapshot remains until RestoreDNS runs.
func PrepareDNS(statePath string, resolverPort int, domains DNSDomains) error {
	switch runtime.GOOS {
	case "darwin":
		return prepareDNS(statePath, resolverPort, domains, execDNSRunner{})
	case "linux":
		return prepareLinuxDNS(statePath, resolverPort, domains, execDNSRunner{})
	default:
		return fmt.Errorf("当前平台不支持 DNS 管理")
	}
}

// RestoreDNS restores a snapshot created by PrepareDNS. It is safe to call when
// no snapshot exists.
func RestoreDNS(statePath string) (bool, error) {
	if state, ok := readDNSStatePlatform(statePath); ok && state.Platform == "linux" {
		return restoreLinuxDNS(statePath, execDNSRunner{})
	}
	if runtime.GOOS == "linux" {
		return restoreLinuxDNS(statePath, execDNSRunner{})
	}
	if runtime.GOOS != "darwin" {
		return false, nil
	}
	return restoreDNS(statePath, execDNSRunner{})
}

// RecoverDNS restores an interrupted run: it applies a leftover snapshot and
// removes any resolver files this application still manages. Without a
// snapshot it never touches the network service's DNS, so a user's manual
// upstream choice is left alone.
func RecoverDNS(statePath string) (bool, error) {
	if state, ok := readDNSStatePlatform(statePath); ok && state.Platform == "linux" {
		return restoreLinuxDNS(statePath, execDNSRunner{})
	}
	if runtime.GOOS == "linux" {
		return restoreLinuxDNS(statePath, execDNSRunner{})
	}
	if runtime.GOOS != "darwin" {
		return false, nil
	}
	return recoverDNS(statePath, execDNSRunner{})
}

func readDNSStatePlatform(statePath string) (DNSState, bool) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return DNSState{}, false
	}
	var state DNSState
	if json.Unmarshal(data, &state) != nil {
		return DNSState{}, false
	}
	return state, true
}

func prepareDNS(statePath string, resolverPort int, domains DNSDomains, runner dnsCommandRunner) error {
	if strings.TrimSpace(statePath) == "" {
		return fmt.Errorf("DNS 状态文件路径为空")
	}
	if _, err := os.Stat(statePath); err == nil {
		return fmt.Errorf("检测到未恢复的 DNS 状态，请先执行恢复")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查 DNS 状态: %w", err)
	}

	state, err := activeDNS(runner)
	if err != nil {
		return err
	}
	managed := resolverDomains(domains.Private)
	resolvers, err := snapshotResolverFiles(managed)
	if err != nil {
		return err
	}
	state.Resolvers = resolvers
	ipv6, err := snapshotIPv6(runner, state.Service)
	if err != nil {
		return err
	}
	state.IPv6 = ipv6
	if err := writeDNSState(statePath, state); err != nil {
		return err
	}
	if err := installResolverFiles(managed, resolverPort); err != nil {
		prepareErr := fmt.Errorf("安装内网域名 resolver: %w", err)
		if _, restoreErr := restoreDNS(statePath, runner); restoreErr != nil {
			return errors.Join(prepareErr, fmt.Errorf("回滚启动前的 DNS: %w", restoreErr))
		}
		return prepareErr
	}
	searchDomains := mergeUnique(state.SearchDomains, domains.Search)
	if !reflectStringSlicesEqual(searchDomains, state.SearchDomains) {
		if err := setSearchDomains(runner, state.Service, searchDomains); err != nil {
			prepareErr := fmt.Errorf("设置内网搜索域: %w", err)
			if _, restoreErr := restoreDNS(statePath, runner); restoreErr != nil {
				return errors.Join(prepareErr, fmt.Errorf("回滚启动前的 DNS: %w", restoreErr))
			}
			return prepareErr
		}
	}
	if !state.Automatic {
		if err := setDNS(runner, state.Service, nil); err != nil {
			prepareErr := fmt.Errorf("清理启动前的 DNS 覆盖: %w", err)
			if _, restoreErr := restoreDNS(statePath, runner); restoreErr != nil {
				return errors.Join(prepareErr, fmt.Errorf("回滚启动前的 DNS: %w", restoreErr))
			}
			return prepareErr
		}
		flushDNSCache(runner)
	}
	if err := limitIPv6(runner, ipv6); err != nil {
		prepareErr := fmt.Errorf("关闭公网 IPv6: %w", err)
		if _, restoreErr := restoreDNS(statePath, runner); restoreErr != nil {
			return errors.Join(prepareErr, fmt.Errorf("回滚启动前的 DNS: %w", restoreErr))
		}
		return prepareErr
	}
	return nil
}

func restoreDNS(statePath string, runner dnsCommandRunner) (bool, error) {
	if strings.TrimSpace(statePath) == "" {
		return false, nil
	}
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
		if errors.Is(err, os.ErrPermission) {
			return false, fmt.Errorf(
				"读取 DNS 状态: %w；该恢复点可能由 sudo 启动的程序创建，请勿删除，先执行 sudo chown \"$(id -u):$(id -g)\" %s 后重试",
				err,
				strconv.Quote(statePath),
			)
		}
		return false, fmt.Errorf("读取 DNS 状态: %w", err)
	}
	var state DNSState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, fmt.Errorf("解析 DNS 状态: %w", err)
	}
	if strings.TrimSpace(state.Service) == "" {
		return false, fmt.Errorf("DNS 状态缺少网络服务")
	}
	servers := state.Servers
	if state.Automatic {
		servers = nil
	} else if len(servers) == 0 {
		return false, fmt.Errorf("DNS 状态缺少原始服务器")
	}
	if err := setDNS(runner, state.Service, servers); err != nil {
		return false, fmt.Errorf("恢复 %s DNS: %w", state.Service, err)
	}
	if state.SearchDomainsCaptured {
		if err := setSearchDomains(runner, state.Service, state.SearchDomains); err != nil {
			return false, fmt.Errorf("恢复 %s 搜索域: %w", state.Service, err)
		}
	}
	if err := restoreIPv6(runner, state.IPv6); err != nil {
		return false, err
	}
	if err := restoreResolverFiles(state.Resolvers); err != nil {
		return false, err
	}
	flushDNSCache(runner)
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("删除已恢复的 DNS 状态: %w", err)
	}
	return true, nil
}

func recoverDNS(statePath string, runner dnsCommandRunner) (bool, error) {
	restored, err := restoreDNS(statePath, runner)
	if err != nil || restored {
		return restored, err
	}
	return false, cleanupManagedResolverFiles()
}

func activeDNS(runner dnsCommandRunner) (DNSState, error) {
	routeOutput, err := runner.Output("route", "-n", "get", "default")
	if err != nil {
		return DNSState{}, commandError("读取默认路由", routeOutput, err)
	}
	iface := parseDefaultRouteInterface(string(routeOutput))
	if iface == "" {
		return DNSState{}, fmt.Errorf("默认路由缺少网络接口")
	}
	orderOutput, err := runner.Output("networksetup", "-listnetworkserviceorder")
	if err != nil {
		return DNSState{}, commandError("读取网络服务", orderOutput, err)
	}
	service := parseNetworkService(string(orderOutput), iface)
	if service == "" {
		return DNSState{}, fmt.Errorf("找不到接口 %s 对应的网络服务", iface)
	}
	dnsOutput, err := runner.Output("networksetup", "-getdnsservers", service)
	if err != nil {
		return DNSState{}, commandError("读取 "+service+" DNS", dnsOutput, err)
	}
	servers, automatic, err := parseDNSServers(string(dnsOutput))
	if err != nil {
		return DNSState{}, fmt.Errorf("读取 %s DNS: %w", service, err)
	}
	searchOutput, err := runner.Output("networksetup", "-getsearchdomains", service)
	if err != nil {
		return DNSState{}, commandError("读取 "+service+" 搜索域", searchOutput, err)
	}
	searchDomains, err := parseSearchDomains(string(searchOutput))
	if err != nil {
		return DNSState{}, fmt.Errorf("读取 %s 搜索域: %w", service, err)
	}
	return DNSState{
		Service:               service,
		Automatic:             automatic,
		Servers:               servers,
		SearchDomains:         searchDomains,
		SearchDomainsCaptured: true,
	}, nil
}

func parseDefaultRouteInterface(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return ""
}

func parseNetworkService(output, iface string) string {
	var service string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "(") && !strings.HasPrefix(line, "(Hardware Port:") {
			if end := strings.Index(line, ")"); end >= 0 {
				service = strings.TrimSpace(line[end+1:])
				service = strings.TrimPrefix(service, "*")
				service = strings.TrimSpace(service)
			}
			continue
		}
		if service != "" && strings.Contains(line, "Device: "+iface+")") {
			return service
		}
	}
	return ""
}

func parseDNSServers(output string) ([]string, bool, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, false, fmt.Errorf("networksetup 返回空结果")
	}
	if strings.Contains(output, "aren't any DNS Servers set on") {
		return nil, true, nil
	}
	var servers []string
	for _, line := range strings.Split(output, "\n") {
		value := strings.TrimSpace(line)
		if net.ParseIP(value) == nil {
			return nil, false, fmt.Errorf("无法识别结果 %q", value)
		}
		servers = append(servers, value)
	}
	if len(servers) == 0 {
		return nil, false, fmt.Errorf("未读取到 DNS 服务器")
	}
	return servers, false, nil
}

func parseSearchDomains(output string) ([]string, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, fmt.Errorf("networksetup 返回空结果")
	}
	if strings.Contains(output, "aren't any Search Domains set on") {
		return nil, nil
	}
	domains := make([]string, 0, strings.Count(output, "\n")+1)
	for _, line := range strings.Split(output, "\n") {
		value := strings.TrimSpace(line)
		if value == "" || strings.ContainsAny(value, " \t") {
			return nil, fmt.Errorf("无法识别结果 %q", value)
		}
		domains = append(domains, value)
	}
	return domains, nil
}

func setDNS(runner dnsCommandRunner, service string, servers []string) error {
	args := []string{"-setdnsservers", service}
	if len(servers) == 0 {
		args = append(args, "Empty")
	} else {
		args = append(args, servers...)
	}
	output, err := runner.PrivilegedOutput("networksetup", args...)
	if err != nil {
		return commandError("设置 DNS", output, err)
	}
	return nil
}

func setSearchDomains(runner dnsCommandRunner, service string, domains []string) error {
	args := []string{"-setsearchdomains", service}
	if len(domains) == 0 {
		args = append(args, "Empty")
	} else {
		args = append(args, domains...)
	}
	output, err := runner.PrivilegedOutput("networksetup", args...)
	if err != nil {
		return commandError("设置搜索域", output, err)
	}
	return nil
}

func mergeUnique(existing, required []string) []string {
	merged := make([]string, 0, len(existing)+len(required))
	seen := make(map[string]bool, len(existing)+len(required))
	for _, values := range [][]string{existing, required} {
		for _, value := range values {
			key := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, value)
		}
	}
	return merged
}

func reflectStringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func snapshotResolverFiles(domains []string) ([]ResolverFileState, error) {
	states := make([]ResolverFileState, 0, len(domains))
	for _, domain := range domains {
		path := resolverPath(domain)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			states = append(states, ResolverFileState{Domain: domain})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取 resolver %s: %w", domain, err)
		}
		states = append(states, ResolverFileState{Domain: domain, Existed: true, Content: string(data)})
	}
	return states, nil
}

func installResolverFiles(domains []string, port int) error {
	if port <= 0 {
		return fmt.Errorf("DNS 端口无效: %d", port)
	}
	if err := ensureResolverDir(); err != nil {
		return err
	}
	for _, domain := range domains {
		if err := writeResolverFile(resolverPath(domain), managedResolverContent(port)); err != nil {
			return fmt.Errorf("写入 resolver %s: %w", domain, err)
		}
	}
	return nil
}

func restoreResolverFiles(states []ResolverFileState) error {
	if len(states) == 0 {
		return cleanupManagedResolverFiles()
	}
	if err := ensureResolverDir(); err != nil {
		return err
	}
	for _, state := range states {
		if !isManagedResolverDomain(state.Domain) {
			return fmt.Errorf("DNS 状态包含非受管 resolver 域名 %q，拒绝恢复", state.Domain)
		}
		path := resolverPath(state.Domain)
		if state.Existed {
			if err := writeResolverFile(path, []byte(state.Content)); err != nil {
				return fmt.Errorf("恢复 resolver %s: %w", state.Domain, err)
			}
			continue
		}
		if err := removeResolverFile(path); err != nil {
			return fmt.Errorf("删除 resolver %s: %w", state.Domain, err)
		}
	}
	return nil
}

// cleanupManagedResolverFiles 删除 /etc/resolver 里所有由本工具写入的文件。
// 没有 DNS 快照时用它兜底；域名清单可能已随设置变化，所以按文件内容的
// 标记识别，而不是按当前设置。
func cleanupManagedResolverFiles() error {
	entries, err := os.ReadDir(resolverDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 %s: %w", resolverDir, err)
	}
	for _, entry := range entries {
		domain := entry.Name()
		if !entry.Type().IsRegular() || !isManagedResolverDomain(domain) {
			continue
		}
		path := resolverPath(domain)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("读取 resolver %s: %w", domain, err)
		}
		if strings.Contains(string(data), "Managed by tizi") {
			if err := removeResolverFile(path); err != nil {
				return fmt.Errorf("清理 resolver %s: %w", domain, err)
			}
		}
	}
	return nil
}

func resolverPath(domain string) string {
	return filepath.Join(resolverDir, domain)
}

// isManagedResolverDomain reports whether domain is a plain domain name that
// may be used as a resolver file name. The DNS state file is user-writable
// while the recovery guard restores it as root, so only bare domain names
// (letters, digits, '-', '_' and inner dots) may reach the filesystem.
func isManagedResolverDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || domain != filepath.Base(domain) || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	for _, r := range domain {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// checkDNSStateFile refuses snapshots that are not private regular files. The
// file may be restored by a root guard, so a symlink or a group/world-writable
// file must not be trusted.
func checkDNSStateFile(statePath string) error {
	info, err := os.Lstat(statePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("DNS 状态 %s 不是普通文件，拒绝恢复", statePath)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("DNS 状态 %s 权限 %04o 过于宽松，请执行 chmod 600 后重试", statePath, perm)
	}
	return nil
}

func managedResolverContent(port int) []byte {
	return []byte(fmt.Sprintf("# Managed by tizi; do not edit.\nnameserver 127.0.0.1\nport %d\ntimeout 3\n", port))
}

func ensureResolverDir() error {
	if !needsPrivilegedResolverOps() {
		return os.MkdirAll(resolverDir, 0o755)
	}
	output, err := exec.Command("sudo", "-n", "mkdir", "-p", resolverDir).CombinedOutput()
	if err != nil {
		return commandError("创建 /etc/resolver", output, err)
	}
	return nil
}

func writeResolverFile(path string, content []byte) error {
	if !needsPrivilegedResolverOps() {
		return os.WriteFile(path, content, 0o644)
	}
	tmp, err := os.CreateTemp("", "tizi-resolver-*")
	if err != nil {
		return fmt.Errorf("创建临时 resolver: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时 resolver: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时 resolver: %w", err)
	}
	output, err := exec.Command("sudo", "-n", "install", "-m", "0644", tmp.Name(), path).CombinedOutput()
	if err != nil {
		return commandError("安装 resolver", output, err)
	}
	return nil
}

func removeResolverFile(path string) error {
	if !needsPrivilegedResolverOps() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	output, err := exec.Command("sudo", "-n", "rm", "-f", path).CombinedOutput()
	if err != nil {
		return commandError("删除 resolver", output, err)
	}
	return nil
}

func needsPrivilegedResolverOps() bool {
	return filepath.Clean(resolverDir) == "/etc/resolver" && os.Geteuid() != 0
}

func writeDNSState(path string, state DNSState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 DNS 状态: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建 DNS 状态目录: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入 DNS 状态: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("设置 DNS 状态权限: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存 DNS 状态: %w", err)
	}
	return nil
}

func flushDNSCache(runner dnsCommandRunner) {
	_ = runner.Run("dscacheutil", "-flushcache")
	_ = runner.Run("sudo", "-n", "killall", "-HUP", "mDNSResponder")
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("%s: %s", action, detail)
}
