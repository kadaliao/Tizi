package claudeblock

import "syscall"

// bindToInterface 让套接字只从指定网卡发出（IP_BOUND_IF），不经过路由表里
// 指向 TUN 的路由。
func bindToInterface(fd, index int) error {
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_BOUND_IF, index)
}
