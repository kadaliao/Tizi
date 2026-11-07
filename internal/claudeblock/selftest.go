package claudeblock

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/kadaliao/tizi/internal/system"
)

// selfTestTimeout 限定自检连接的等待时间。规则生效时 pf 立即拒绝，只有没
// 生效时才会等满。
const selfTestTimeout = 1500 * time.Millisecond

// ErrSelfTestOffline 表示没有物理网卡的默认路由，无法自检。
var ErrSelfTestOffline = errors.New("没有可用的网络，无法自检")

// SelfTest 从物理网卡向 SelfTestAddress 发起一次 TCP 连接：规则生效时 pf
// 立即拒绝（连接被重置），否则连接超时或得到别的结果。它不接触 Anthropic
// 的地址，也不需要管理员权限。
func SelfTest() error {
	if !Supported() {
		return errUnsupported
	}
	name := physicalInterface(system.DefaultRoute())
	if name == "" {
		return ErrSelfTestOffline
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("读取网卡 %s: %w", name, err)
	}
	return dialBlocked(iface.Index)
}

// physicalInterface 从 system.DefaultRoute 的结果（如 "en1 via 192.168.2.1"）
// 取出网卡名。
func physicalInterface(route string) string {
	if route == "" || route == system.RouteUnavailable || route == system.RouteUnsupported {
		return ""
	}
	name, _, _ := strings.Cut(route, " ")
	return name
}

// dialBlocked 把连接绑定到指定网卡（绕过 TUN 的路由），确认它被拒绝。
func dialBlocked(interfaceIndex int) error {
	dialer := net.Dialer{
		Timeout: selfTestTimeout,
		Control: func(_, _ string, raw syscall.RawConn) error {
			var bindErr error
			if err := raw.Control(func(fd uintptr) {
				bindErr = bindToInterface(int(fd), interfaceIndex)
			}); err != nil {
				return err
			}
			return bindErr
		},
	}
	conn, err := dialer.Dial("tcp4", net.JoinHostPort(SelfTestAddress, "443"))
	if err == nil {
		_ = conn.Close()
		return errors.New("自检地址可以连通，防火墙规则没有生效")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	return fmt.Errorf("防火墙规则没有生效（自检连接 %s）", describeDialError(err))
}

func describeDialError(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "超时"
	}
	return err.Error()
}
