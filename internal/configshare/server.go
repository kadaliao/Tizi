package configshare

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultLifetime = 5 * time.Minute

// Session temporarily exposes one generated Mihomo configuration on the LAN.
// The unguessable URL is the only credential; callers must close the session
// as soon as the QR-code import finishes.
type Session struct {
	URL       string
	ExpiresAt time.Time

	server *http.Server
	done   chan struct{}
	once   sync.Once
	timer  *time.Timer
}

// Start reads configPath once and serves that immutable snapshot for five
// minutes on the most likely physical LAN interface.
func Start(configPath string) (*Session, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("读取待分享配置: %w", err)
	}
	address, err := lanIPv4()
	if err != nil {
		return nil, err
	}
	return start(data, address.String(), defaultLifetime)
}

// Done is closed when the listener stops, whether explicitly or at expiry.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// Close immediately stops sharing the configuration.
func (s *Session) Close() error {
	return s.close(true)
}

func (s *Session) close(stopTimer bool) error {
	if s == nil {
		return nil
	}
	// 过期回调不能读取 timer：极短 lifetime 下，回调可能先于
	// time.AfterFunc 返回并赋值，与这里的读取形成数据竞争。
	if stopTimer && s.timer != nil {
		s.timer.Stop()
	}
	err := s.server.Close()
	s.finish()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Session) finish() {
	s.once.Do(func() { close(s.done) })
}

func start(data []byte, host string, lifetime time.Duration) (*Session, error) {
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("启动局域网配置分享: %w", err)
	}
	tokenBytes := make([]byte, 18)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("生成分享地址: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	requestPath := "/" + token + "/config.yaml"
	mux := http.NewServeMux()
	mux.HandleFunc(requestPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", `attachment; filename="tizi.yaml"`)
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Profile-Title", "Tizi")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	expiresAt := time.Now().Add(lifetime)
	session := &Session{
		URL:       "http://" + listener.Addr().String() + requestPath,
		ExpiresAt: expiresAt,
		server:    server,
		done:      make(chan struct{}),
	}
	session.timer = time.AfterFunc(lifetime, func() { _ = session.close(false) })
	go func() {
		_ = server.Serve(listener)
		session.finish()
	}()
	return session, nil
}

type addressCandidate struct {
	address netip.Addr
	name    string
	score   int
}

func lanIPv4() (netip.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("读取网络接口: %w", err)
	}
	var candidates []addressCandidate
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, value := range addresses {
			prefix, parseErr := netip.ParsePrefix(value.String())
			if parseErr != nil {
				continue
			}
			address := prefix.Addr().Unmap()
			if !address.Is4() || !address.IsPrivate() {
				continue
			}
			candidates = append(candidates, addressCandidate{
				address: address,
				name:    iface.Name,
				score:   interfaceScore(iface.Name, address),
			})
		}
	}
	if len(candidates) == 0 {
		return netip.Addr{}, fmt.Errorf("未找到局域网 IPv4；请让电脑和手机连接同一 Wi-Fi 后重试")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].name != candidates[j].name {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].address.Less(candidates[j].address)
	})
	return candidates[0].address, nil
}

func interfaceScore(name string, address netip.Addr) int {
	name = strings.ToLower(name)
	score := 0
	for _, prefix := range []string{"en0", "en1", "eth", "enp", "wlan", "wl"} {
		if strings.HasPrefix(name, prefix) {
			score += 100
			break
		}
	}
	for _, marker := range []string{"utun", "tun", "tap", "wg", "tailscale", "docker", "veth", "vmnet", "bridge", "mihomo", "clash"} {
		if strings.Contains(name, marker) {
			score -= 100
			break
		}
	}
	if address.Is4() {
		octets := address.As4()
		switch {
		case octets[0] == 192 && octets[1] == 168:
			score += 20
		case octets[0] == 172 && octets[1] >= 16 && octets[1] <= 31:
			score += 10
		case octets[0] == 10:
			score += 5
		}
	}
	return score
}
