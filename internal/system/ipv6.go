package system

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Mihomo 的 TUN 只接管 IPv4（ipv6: false）。系统若仍有 IPv6 默认路由，访问
// 双栈海外站点的应用会直接从物理网卡走 IPv6，完全绕过代理，而且暴露真实
// 地址。连接期间把物理网卡的公网 IPv6 关掉，应用只剩 IPv4 可选；快照写进
// DNS 恢复点，停止、崩溃恢复时一并还原。

const (
	ipv6ModeAutomatic = "automatic"
	ipv6ModeManual    = "manual"
	ipv6ModeLinkLocal = "link-local"
	ipv6ModeOff       = "off"
	// ipv6ModeRestricted 表示 getinfo 没有 IPv6: 一行：「仅本地链路」时就是
	// 这样，不拿公网 IPv6，无需再限制。
	ipv6ModeRestricted = "restricted"
	// ipv6ModeEnabled 是 Linux 上 disable_ipv6=0 的快照值。
	ipv6ModeEnabled = "enabled"
)

// IPv6ServiceState 记录一个被本工具限制过 IPv6 的 macOS 网络服务或 Linux
// 网卡在限制之前的配置。
type IPv6ServiceState struct {
	Service      string `json:"service"`
	Mode         string `json:"mode"`
	Address      string `json:"address,omitempty"`
	PrefixLength int    `json:"prefix_length,omitempty"`
	Router       string `json:"router,omitempty"`
}

var (
	sysClassNet = "/sys/class/net"
	procIPv6Dir = "/proc/sys/net/ipv6/conf"

	linuxInterfaceName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,15}$`)
)

type networkService struct {
	Name     string
	Device   string
	Disabled bool
}

// parseNetworkServices 解析 networksetup -listnetworkserviceorder，返回每个
// 网络服务及其设备名。VPN 类服务（如 Tailscale）的 Device 为空。
func parseNetworkServices(output string) []networkService {
	var services []networkService
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "(") {
			continue
		}
		if strings.HasPrefix(line, "(Hardware Port:") {
			if len(services) == 0 {
				continue
			}
			if at := strings.Index(line, "Device: "); at >= 0 {
				device := line[at+len("Device: "):]
				device = strings.TrimSpace(strings.TrimSuffix(device, ")"))
				services[len(services)-1].Device = device
			}
			continue
		}
		end := strings.Index(line, ")")
		if end < 0 {
			continue
		}
		marker := strings.TrimSpace(line[1:end])
		name := strings.TrimSpace(line[end+1:])
		disabled := marker == "*"
		if strings.HasPrefix(name, "*") {
			disabled = true
			name = strings.TrimSpace(strings.TrimPrefix(name, "*"))
		}
		if name != "" {
			services = append(services, networkService{Name: name, Disabled: disabled})
		}
	}
	return services
}

// hardwareServices 返回已启用且带物理设备的网络服务。
func hardwareServices(runner dnsCommandRunner) ([]networkService, error) {
	output, err := runner.Output("networksetup", "-listnetworkserviceorder")
	if err != nil {
		return nil, commandError("读取网络服务", output, err)
	}
	var result []networkService
	for _, service := range parseNetworkServices(string(output)) {
		if service.Device != "" && !service.Disabled {
			result = append(result, service)
		}
	}
	return result, nil
}

// parseIPv6Info 解析 networksetup -getinfo 里的 IPv6 配置。
func parseIPv6Info(output string) (IPv6ServiceState, error) {
	var state IPv6ServiceState
	found, sawFields := false, false
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "IPv6":
			found = true
			lower := strings.ToLower(value)
			switch {
			case strings.Contains(lower, "off"):
				state.Mode = ipv6ModeOff
			case strings.Contains(lower, "link"):
				state.Mode = ipv6ModeLinkLocal
			case strings.Contains(lower, "manual"):
				state.Mode = ipv6ModeManual
			case strings.Contains(lower, "auto"):
				state.Mode = ipv6ModeAutomatic
			default:
				return IPv6ServiceState{}, fmt.Errorf("无法识别 IPv6 模式 %q", value)
			}
		case "IPv6 IP address":
			sawFields = true
			if value != "none" {
				state.Address = value
			}
		case "IPv6 Prefix Length":
			state.PrefixLength, _ = strconv.Atoi(value)
		case "IPv6 Router":
			sawFields = true
			if value != "none" {
				state.Router = value
			}
		}
	}
	if !found {
		if sawFields {
			return IPv6ServiceState{Mode: ipv6ModeRestricted}, nil
		}
		return IPv6ServiceState{}, fmt.Errorf("networksetup 未返回 IPv6 配置")
	}
	return state, nil
}

// restorableManual 报告手动配置是否完整到足以原样还原。
func (s IPv6ServiceState) restorableManual() bool {
	return net.ParseIP(s.Address) != nil && net.ParseIP(s.Router) != nil &&
		s.PrefixLength > 0 && s.PrefixLength <= 128
}

// snapshotIPv6 读取每个已启用物理网络服务的 IPv6 配置，只返回需要被限制
// （也就是当前仍有公网 IPv6）的服务。defaultService 是承载默认路由的服务，
// 它读不出或无法还原时直接报错，以免静默留下泄漏；其他服务读不出就跳过。
func snapshotIPv6(runner dnsCommandRunner, defaultService string) ([]IPv6ServiceState, error) {
	services, err := hardwareServices(runner)
	if err != nil {
		return nil, err
	}
	var states []IPv6ServiceState
	for _, service := range services {
		critical := service.Name == defaultService
		output, err := runner.Output("networksetup", "-getinfo", service.Name)
		if err != nil {
			if critical {
				return nil, commandError("读取 "+service.Name+" IPv6", output, err)
			}
			continue
		}
		state, err := parseIPv6Info(string(output))
		if err != nil {
			if critical {
				return nil, fmt.Errorf("读取 %s IPv6: %w", service.Name, err)
			}
			continue
		}
		state.Service = service.Name
		switch state.Mode {
		case ipv6ModeAutomatic:
		case ipv6ModeManual:
			if !state.restorableManual() {
				if critical {
					return nil, fmt.Errorf("%s 使用手动 IPv6 配置但信息不完整，无法在断开后还原", service.Name)
				}
				continue
			}
		default:
			continue
		}
		states = append(states, state)
	}
	return states, nil
}

// limitIPv6 把快照中的服务改为「仅本地链路」：保留 fe80:: 地址（本地网络
// 发现仍可用），但不再获取公网 IPv6 地址和默认路由。
func limitIPv6(runner dnsCommandRunner, states []IPv6ServiceState) error {
	for _, state := range states {
		output, err := runner.PrivilegedOutput("networksetup", "-setv6LinkLocal", state.Service)
		if err != nil {
			return commandError("限制 "+state.Service+" 的 IPv6", output, err)
		}
	}
	return nil
}

// restoreIPv6 还原 limitIPv6 之前的配置。DNS 状态文件由普通用户可写、由
// root 守护进程还原，所以只处理当前仍存在的物理服务，并校验每个字段。
func restoreIPv6(runner dnsCommandRunner, states []IPv6ServiceState) error {
	if len(states) == 0 {
		return nil
	}
	services, err := hardwareServices(runner)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(services))
	for _, service := range services {
		present[service.Name] = true
	}
	for _, state := range states {
		if !present[state.Service] {
			continue
		}
		var args []string
		switch state.Mode {
		case ipv6ModeAutomatic:
			args = []string{"-setv6automatic", state.Service}
		case ipv6ModeManual:
			if !state.restorableManual() {
				return fmt.Errorf("IPv6 状态对 %s 的手动配置无效，拒绝恢复", state.Service)
			}
			args = []string{"-setv6manual", state.Service, state.Address, strconv.Itoa(state.PrefixLength), state.Router}
		default:
			return fmt.Errorf("IPv6 状态包含未知模式 %q，拒绝恢复", state.Mode)
		}
		if output, err := runner.PrivilegedOutput("networksetup", args...); err != nil {
			return commandError("恢复 "+state.Service+" 的 IPv6", output, err)
		}
	}
	return nil
}

// snapshotLinuxIPv6 返回当前仍启用 IPv6 的物理网卡。虚拟网卡（docker、
// 网桥、TUN）没有 device 目录，不动它们。
func snapshotLinuxIPv6() []IPv6ServiceState {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil
	}
	var states []IPv6ServiceState
	for _, entry := range entries {
		name := entry.Name()
		if !linuxInterfaceName.MatchString(name) || name == LinuxTUNInterface {
			continue
		}
		if _, err := os.Stat(filepath.Join(sysClassNet, name, "device")); err != nil {
			continue
		}
		value, err := os.ReadFile(filepath.Join(procIPv6Dir, name, "disable_ipv6"))
		if err != nil || strings.TrimSpace(string(value)) != "0" {
			continue
		}
		states = append(states, IPv6ServiceState{Service: name, Mode: ipv6ModeEnabled})
	}
	return states
}

func setLinuxIPv6(runner dnsCommandRunner, iface string, disable bool) error {
	value := "0"
	if disable {
		value = "1"
	}
	output, err := runner.PrivilegedOutput("sysctl", "-w", "net.ipv6.conf."+iface+".disable_ipv6="+value)
	if err != nil {
		return commandError("设置 "+iface+" 的 IPv6", output, err)
	}
	return nil
}

func limitLinuxIPv6(runner dnsCommandRunner, states []IPv6ServiceState) error {
	for _, state := range states {
		if err := setLinuxIPv6(runner, state.Service, true); err != nil {
			return err
		}
	}
	return nil
}

func restoreLinuxIPv6(runner dnsCommandRunner, states []IPv6ServiceState) error {
	for _, state := range states {
		if state.Mode != ipv6ModeEnabled || !linuxInterfaceName.MatchString(state.Service) {
			return fmt.Errorf("IPv6 状态包含无效网卡 %q，拒绝恢复", state.Service)
		}
		if _, err := os.Stat(filepath.Join(sysClassNet, state.Service)); err != nil {
			continue
		}
		if err := setLinuxIPv6(runner, state.Service, false); err != nil {
			return err
		}
	}
	return nil
}

// IPv6DefaultRoute 返回承载系统 IPv6 默认路由的网络接口；没有 IPv6 默认
// 路由时返回空字符串。
func IPv6DefaultRoute() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		// 没有 IPv6 默认路由时 route 仍以 0 退出，只在 stderr 报 not in table，
		// stdout 里没有 interface 行。
		output, err := inspectOutput("route", "-n", "get", "-inet6", "default")
		if err != nil {
			return "", err
		}
		return parseDefaultRouteInterface(string(output)), nil
	case "linux":
		output, err := inspectOutput("ip", "-6", "route", "show", "default")
		if err != nil {
			return "", err
		}
		return linuxDefaultRouteInterface(string(output)), nil
	default:
		return "", fmt.Errorf("当前平台不支持")
	}
}
