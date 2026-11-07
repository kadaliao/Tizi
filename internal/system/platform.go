package system

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const LinuxTUNInterface = "tizi"

var osReleasePath = "/etc/os-release"

func PlatformSupport() (bool, string) {
	switch runtime.GOOS {
	case "darwin":
		return true, "macOS，支持完整 TUN 模式"
	case "linux":
		if !isSupportedUbuntu(osReleasePath) {
			return false, "首版仅支持 Ubuntu 22.04/24.04"
		}
		// xdg-open 只用来打开日志目录和看板，Ubuntu Server 没有它也能连接。
		for _, command := range []string{"ip", "resolvectl", "sudo"} {
			if _, err := exec.LookPath(command); err != nil {
				return false, fmt.Sprintf("Ubuntu 缺少 %s", command)
			}
		}
		if _, err := os.Stat("/dev/net/tun"); err != nil {
			return false, "Ubuntu 缺少 /dev/net/tun"
		}
		return true, "Ubuntu，支持完整 TUN 模式"
	default:
		return false, "当前版本支持 macOS 和 Ubuntu"
	}
}

func ValidateTUNPlatform() error {
	if supported, detail := PlatformSupport(); !supported {
		return fmt.Errorf("%s", detail)
	}
	if runtime.GOOS == "linux" {
		if output, err := exec.Command("resolvectl", "status").CombinedOutput(); err != nil {
			detail := strings.TrimSpace(string(output))
			if detail == "" {
				detail = err.Error()
			}
			// 常见原因是 dbus 重启过而 systemd-resolved 没有重新注册，
			// resolvectl 会报 Failed to activate service ... timed out。
			return fmt.Errorf("Ubuntu systemd-resolved 不可用: %s；可运行 sudo systemctl restart systemd-resolved 后重试", detail)
		}
	}
	return nil
}

func isSupportedUbuntu(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return strings.EqualFold(values["ID"], "ubuntu") &&
		(values["VERSION_ID"] == "22.04" || values["VERSION_ID"] == "24.04")
}
