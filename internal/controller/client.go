package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
}

// reloadTimeout 限定一次配置重载的等待时间。
const reloadTimeout = 15 * time.Second

type Group struct {
	Name    string
	Now     string
	Choices []string
}

// Overview 是 /proxies 的摘要。
type Overview struct {
	// Groups 是可以手动切换的策略组（Selector）。
	Groups []Group
	// Auto 是 Mihomo 自己挑节点的策略组（url-test、fallback、load-balance），
	// Now 是它此刻使用的节点，Choices 是参与挑选的全部节点。
	Auto []Group
	// Delays 是节点最近一次测速的延迟（毫秒），0 表示没测通；还没测过的
	// 节点不在里面。
	Delays map[string]int
}

// AllGroups 返回全部策略组，ResolveRoute 用它把「自动选择」展开到实际节点。
func (o Overview) AllGroups() []Group {
	return append(append([]Group(nil), o.Groups...), o.Auto...)
}

type Rule struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Proxy   string `json:"proxy"`
}

type Connection struct {
	ID       string   `json:"id"`
	Chains   []string `json:"chains"`
	Rule     string   `json:"rule"`
	Payload  string   `json:"rulePayload"`
	Download int64    `json:"download"`
	Upload   int64    `json:"upload"`
	Metadata struct {
		Host            string `json:"host"`
		SniffHost       string `json:"sniffHost"`
		SourceIP        string `json:"sourceIP"`
		SourcePort      string `json:"sourcePort"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
		InboundPort     string `json:"inboundPort"`
		Process         string `json:"process"`
		ProcessPath     string `json:"processPath"`
	} `json:"metadata"`
}

type probeIdentity struct {
	sourceIP        string
	sourcePort      string
	host            string
	destinationPort int
	mixedPort       int
}

func New(port int, secret string) Client {
	return Client{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		Secret:  secret,
		HTTP:    &http.Client{Timeout: 2 * time.Second},
	}
}

// DashboardURL 生成带 metacubexd 深链参数的面板地址；
// 前端 autoLogin 读取 hash 内的 hostname/port/secret 自动连接，避免手动填 secret。
func DashboardURL(port int, secret string) string {
	return fmt.Sprintf(
		"http://127.0.0.1:%d/ui/#/setup?hostname=127.0.0.1&port=%d&secret=%s",
		port, port, url.QueryEscape(secret),
	)
}

func (c Client) Healthy() bool {
	request, err := c.request(http.MethodGet, c.BaseURL+"/version", nil)
	if err != nil {
		return false
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func (c Client) Groups() ([]Group, error) {
	overview, err := c.Overview("")
	return overview.Groups, err
}

// autoGroupTypes 是 Mihomo 自己挑节点、不能手动切换的策略组类型。
var autoGroupTypes = map[string]bool{"URLTest": true, "Fallback": true, "LoadBalance": true}

type delayRecord struct {
	Delay int `json:"delay"`
}

type delayState struct {
	Alive   bool          `json:"alive"`
	History []delayRecord `json:"history"`
}

// Overview 读取策略组和节点延迟。testURL 是测速地址：延迟取这个地址最近
// 一次的结果；为空时不读延迟。
func (c Client) Overview(testURL string) (Overview, error) {
	request, err := c.request(http.MethodGet, c.BaseURL+"/proxies", nil)
	if err != nil {
		return Overview{}, err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return Overview{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Overview{}, fmt.Errorf("Controller 返回 HTTP %s", response.Status)
	}
	var payload struct {
		Proxies map[string]struct {
			Type  string                `json:"type"`
			Now   string                `json:"now"`
			All   []string              `json:"all"`
			Extra map[string]delayState `json:"extra"`
		} `json:"proxies"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&payload); err != nil {
		return Overview{}, err
	}
	overview := Overview{Delays: map[string]int{}}
	for name, proxy := range payload.Proxies {
		group := Group{Name: name, Now: proxy.Now, Choices: proxy.All}
		switch {
		case name == "GLOBAL":
			// GLOBAL 聚合全部代理，是噪音。
		case proxy.Type == "Selector":
			if len(proxy.All) > 0 {
				overview.Groups = append(overview.Groups, group)
			}
		case autoGroupTypes[proxy.Type]:
			if len(proxy.All) > 0 {
				overview.Auto = append(overview.Auto, group)
			}
		case testURL != "":
			if state, ok := proxy.Extra[testURL]; ok && len(state.History) > 0 {
				delay := state.History[len(state.History)-1].Delay
				if !state.Alive {
					delay = 0
				}
				overview.Delays[name] = delay
			}
		}
	}
	sort.Slice(overview.Groups, func(i, j int) bool { return overview.Groups[i].Name < overview.Groups[j].Name })
	sort.Slice(overview.Auto, func(i, j int) bool { return overview.Auto[i].Name < overview.Auto[j].Name })
	return overview, nil
}

// Rules returns the active, ordered routing rules from Mihomo.
func (c Client) Rules() ([]Rule, error) {
	request, err := c.request(http.MethodGet, c.BaseURL+"/rules", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Controller 返回 HTTP %s", response.Status)
	}
	var payload struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Rules, nil
}

// Connections returns Mihomo's currently active connections, including their
// matched rule and resolved proxy chain.
func (c Client) Connections() ([]Connection, error) {
	request, err := c.request(http.MethodGet, c.BaseURL+"/connections", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Controller 返回 HTTP %s", response.Status)
	}
	var payload struct {
		Connections []Connection `json:"connections"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Connections, nil
}

// ProbeRoute opens a CONNECT tunnel through Mihomo's mixed proxy and reads the
// resulting active connection. It sends no HTTP request or application data to
// the destination; Mihomo still performs its complete DNS and rule evaluation.
func (c Client) ProbeRoute(mixedPort int, host string, port int) (Connection, error) {
	tunnel, connection, err := c.OpenProbe(mixedPort, host, port)
	if tunnel != nil {
		_ = tunnel.Close()
	}
	return connection, err
}

// OpenProbe 经 Mihomo 混合代理端口建立到 host:port 的 CONNECT 隧道，并找出
// Mihomo 为它记录的活动连接（含命中的规则和出口链）。隧道建立成功时返回的
// net.Conn 非空，调用方负责关闭，可以继续在上面收发数据；此时 err 非空表示
// 只是没找到连接记录。隧道建立失败时 net.Conn 为 nil。
func (c Client) OpenProbe(mixedPort int, host string, port int) (net.Conn, Connection, error) {
	before, err := c.Connections()
	if err != nil {
		return nil, Connection{}, fmt.Errorf("读取连接列表: %w", err)
	}
	known := make(map[string]bool, len(before))
	for _, connection := range before {
		known[connection.ID] = true
	}

	proxyAddress := fmt.Sprintf("127.0.0.1:%d", mixedPort)
	conn, err := net.DialTimeout("tcp", proxyAddress, 2*time.Second)
	if err != nil {
		return nil, Connection{}, fmt.Errorf("连接本机代理 %s: %w", proxyAddress, err)
	}
	fail := func(err error) (net.Conn, Connection, error) {
		_ = conn.Close()
		return nil, Connection{}, err
	}
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	sourceIP, sourcePort, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return fail(fmt.Errorf("读取路线探测端口: %w", err))
	}
	probe := probeIdentity{
		sourceIP:        sourceIP,
		sourcePort:      sourcePort,
		host:            host,
		destinationPort: port,
		mixedPort:       mixedPort,
	}
	destination := net.JoinHostPort(host, strconv.Itoa(port))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n\r\n", destination, destination); err != nil {
		return fail(fmt.Errorf("发送路线探测: %w", err))
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return fail(fmt.Errorf("目标连接失败: %w", err))
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return fail(fmt.Errorf("目标连接失败: HTTP %s", response.Status))
	}
	_ = conn.SetDeadline(time.Time{})
	tunnel := &bufferedConn{Conn: conn, reader: reader}

	// The successful tunnel remains open until the caller closes it. Its local
	// ephemeral port is the most reliable tracker key; hostname metadata can be
	// empty or populated after the tracker first appears.
	deadline := time.Now().Add(2 * time.Second)
	var lastListErr error
	for time.Now().Before(deadline) {
		connections, listErr := c.Connections()
		if listErr != nil {
			lastListErr = listErr
		} else {
			lastListErr = nil
			for _, candidate := range connections {
				if matchesProbeConnection(candidate, known, probe) {
					return tunnel, candidate, nil
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastListErr != nil {
		return tunnel, Connection{}, fmt.Errorf("Mihomo 已连接目标，但读取活动连接失败: %w", lastListErr)
	}
	return tunnel, Connection{}, fmt.Errorf("Mihomo 已连接目标，但未返回对应的活动连接记录")
}

// bufferedConn 先读出 CONNECT 响应之后已经缓冲的字节，再读底层连接。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func matchesProbeConnection(candidate Connection, known map[string]bool, probe probeIdentity) bool {
	if probe.sourcePort != "" && candidate.Metadata.SourcePort == probe.sourcePort {
		if candidate.Metadata.SourceIP == "" && candidate.Metadata.InboundPort == "" {
			return true
		}
		if candidate.Metadata.SourceIP == probe.sourceIP || candidate.Metadata.InboundPort == strconv.Itoa(probe.mixedPort) {
			return true
		}
	}
	if known[candidate.ID] {
		return false
	}
	if candidate.Metadata.DestinationPort != "" && candidate.Metadata.DestinationPort != strconv.Itoa(probe.destinationPort) {
		return false
	}
	host := strings.TrimSuffix(probe.host, ".")
	for _, candidateHost := range []string{candidate.Metadata.Host, candidate.Metadata.SniffHost} {
		if strings.EqualFold(strings.TrimSuffix(candidateHost, "."), host) {
			return true
		}
	}
	return false
}

// MatchHost returns the first domain rule Mihomo will apply to host. The
// configured rules are ordered, so the first matching rule owns the route.
func MatchHost(rules []Rule, host string) (Rule, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, rule := range rules {
		payload := strings.TrimSpace(rule.Payload)
		ruleType := strings.ReplaceAll(strings.ToUpper(rule.Type), "-", "")
		switch ruleType {
		case "DOMAIN":
			if host == strings.TrimSuffix(strings.ToLower(payload), ".") {
				return rule, true
			}
		case "DOMAINSUFFIX":
			suffix := strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(payload), "."), ".")
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return rule, true
			}
		case "DOMAINKEYWORD":
			if strings.Contains(host, strings.ToLower(payload)) {
				return rule, true
			}
		case "MATCH":
			return rule, true
		case "AND", "OR", "NOT":
			// Generated compound rules only constrain network, port, or IP. They
			// cannot match a hostname by themselves, so continue to the domain
			// rules below them. A hand-written compound domain rule remains
			// indeterminate because evaluating its boolean expression is outside
			// this host-only preview.
			normalizedPayload := strings.ReplaceAll(strings.ToUpper(payload), "-", "")
			if strings.Contains(normalizedPayload, "DOMAIN") ||
				strings.Contains(normalizedPayload, "GEOSITE") ||
				strings.Contains(normalizedPayload, "RULESET") {
				return Rule{}, false
			}
			continue
		case "IPCIDR", "IPCIDR6", "SRCIPCIDR", "DSTPORT", "SRCPORT", "NETWORK", "INPORT", "INBOUND", "PROCESSNAME", "PROCESSPATH", "UID":
			// These rules need connection metadata and do not identify a domain.
			// Skip them so a later explicit domain rule can still be previewed.
			continue
		default:
			// GEOIP, GEOSITE, RULE-SET and unknown rule types may match the host,
			// but require DNS, databases, or runtime context. Do not claim a later
			// fallback route when the actual outcome is unknown.
			return Rule{}, false
		}
	}
	return Rule{}, false
}

func (c Client) Select(group, choice string) error {
	body, err := json.Marshal(map[string]string{"name": choice})
	if err != nil {
		return err
	}
	endpoint := c.BaseURL + "/proxies/" + url.PathEscape(group)
	request, err := c.request(http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("切换策略失败: HTTP %s", response.Status)
	}
	return nil
}

// ReloadConfig 让 Mihomo 重新解析配置文件并重建全部 outbound，其中包括
// OpenVPN 会话。force=true 保证文件内容未变时也会真正重载，因此它可以在不
// 结束 Mihomo 进程的前提下替换掉一个已经失效、但自身迟迟不重连的隧道。
// path 必须位于 Mihomo 允许的路径（通常是用户家目录）内，否则核心会拒绝。
func (c Client) ReloadConfig(path string) error {
	body, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return err
	}
	endpoint := c.BaseURL + "/configs?force=true"
	request, err := c.request(http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	// 重载要重新解析 YAML、重建监听与 outbound，比普通探测慢，因此不复用
	// Client.HTTP 的 2s 探测超时；超时中断也不代表核心没有完成重载。
	httpClient := *c.HTTP
	httpClient.Timeout = reloadTimeout
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		if detail := strings.TrimSpace(string(message)); detail != "" {
			return fmt.Errorf("重载配置失败: HTTP %s: %s", response.Status, detail)
		}
		return fmt.Errorf("重载配置失败: HTTP %s", response.Status)
	}
	return nil
}

func (c Client) CloseConnections() error {
	request, err := c.request(http.MethodDelete, c.BaseURL+"/connections", nil)
	if err != nil {
		return err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("关闭连接失败: HTTP %s", response.Status)
	}
	return nil
}

// CloseConnection closes one active Mihomo connection. A missing connection is
// already in the desired state because entries can disappear between listing
// and deletion.
func (c Client) CloseConnection(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("连接 ID 不能为空")
	}
	request, err := c.request(http.MethodDelete, c.BaseURL+"/connections/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("关闭连接失败: HTTP %s", response.Status)
	}
	return nil
}

// CloseConnectionsMatching snapshots active connections and closes only those
// selected by match. It continues after individual failures so one stale entry
// cannot prevent the remaining matches from being refreshed.
func (c Client) CloseConnectionsMatching(match func(Connection) bool) (int, error) {
	if match == nil {
		return 0, fmt.Errorf("连接筛选器不能为空")
	}
	connections, err := c.Connections()
	if err != nil {
		return 0, err
	}
	closed := 0
	failed := 0
	var firstCloseError error
	for _, connection := range connections {
		if connection.ID == "" || !match(connection) {
			continue
		}
		if err := c.CloseConnection(connection.ID); err != nil {
			failed++
			if firstCloseError == nil {
				firstCloseError = fmt.Errorf("%s: %w", connection.ID, err)
			}
			continue
		}
		closed++
	}
	if failed > 0 {
		return closed, fmt.Errorf("%d 条连接关闭失败（首个错误: %w）", failed, firstCloseError)
	}
	return closed, nil
}

// ProxyDelay measures the round-trip latency through one proxy using Mihomo's
// latency API. probeURL is fetched through the proxy, so a short timeout turns
// this into a fast health check for a stale VPN session instead of a full test.
func (c Client) ProxyDelay(name, probeURL string, timeout time.Duration) (int, error) {
	var payload struct {
		Delay int `json:"delay"`
	}
	if err := c.delayRequest("/proxies/"+url.PathEscape(name)+"/delay", probeURL, timeout, &payload); err != nil {
		return 0, err
	}
	return payload.Delay, nil
}

// GroupDelay 让 Mihomo 同时测一遍策略组里的全部节点，返回测通的节点和延迟
// （毫秒），没测通的不在结果里。测 url-test 这类自动挑选的组时，Mihomo 会
// 清掉固定的选择，按这次的结果重新挑节点。
func (c Client) GroupDelay(name, probeURL string, timeout time.Duration) (map[string]int, error) {
	delays := map[string]int{}
	err := c.delayRequest("/group/"+url.PathEscape(name)+"/delay", probeURL, timeout, &delays)
	if errors.Is(err, errNoDelay) {
		return map[string]int{}, nil
	}
	return delays, err
}

// Unfix 取消自动挑选类策略组（url-test、fallback）的固定选择，并让它按
// 最新的测速结果重新挑节点。
func (c Client) Unfix(group string) error {
	request, err := c.request(http.MethodDelete, c.BaseURL+"/proxies/"+url.PathEscape(group), nil)
	if err != nil {
		return err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("重新挑选节点失败: HTTP %s", response.Status)
	}
	return nil
}

// errNoDelay 是 Mihomo 测速没有测通：超时（HTTP 504）或连接出错（HTTP 503）。
// 测策略组时表示组里一个节点都没测通。
var errNoDelay = errors.New("测不通")

// delayRequest 调用 Mihomo 的测速接口。Mihomo 自己按 timeout 结束测速，
// HTTP 等待时间要比它长一些，不能用 Client.HTTP 的 2 秒探测超时。
func (c Client) delayRequest(path, probeURL string, timeout time.Duration, target any) error {
	query := url.Values{}
	query.Set("timeout", strconv.Itoa(int(timeout.Milliseconds())))
	query.Set("url", probeURL)
	request, err := c.request(http.MethodGet, c.BaseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	httpClient := http.Client{Timeout: timeout + 3*time.Second}
	if c.HTTP != nil {
		httpClient.Transport = c.HTTP.Transport
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusGatewayTimeout, http.StatusServiceUnavailable:
		return errNoDelay
	default:
		return fmt.Errorf("探测延迟失败: HTTP %s", response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(target)
}

// TestDelays 测一遍可以上外网的节点，返回节点名到延迟（毫秒）的对应，0 表示
// 没测通。先测自动挑选的策略组：它包含订阅和节点文件里的全部节点，测完后
// Mihomo 会按新结果重新挑节点；同时单独测不在这些组里的节点（住宅节点、
// 内网 HTTP 出口等）。skipGroups 里的策略组（内网、Tailscale）不上外网，
// 测 probeURL 没有意义；skip 返回 true 的节点（订阅里的说明条目）也跳过。
func (c Client) TestDelays(probeURL string, timeout time.Duration, skipGroups []string, skip func(name string) bool) (map[string]int, error) {
	overview, err := c.Overview("")
	if err != nil {
		return nil, err
	}
	groupNames := map[string]bool{}
	for _, group := range overview.AllGroups() {
		groupNames[group.Name] = true
	}
	skipped := map[string]bool{}
	for _, name := range skipGroups {
		skipped[name] = true
	}
	special := map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true}

	var (
		mu      sync.Mutex
		results = map[string]int{}
		wg      sync.WaitGroup
		errs    []error
	)
	record := func(name string, delay int) {
		mu.Lock()
		defer mu.Unlock()
		if current, ok := results[name]; !ok || delay > current {
			results[name] = delay
		}
	}
	covered := map[string]bool{}
	for _, group := range overview.Auto {
		if skipped[group.Name] {
			continue
		}
		for _, name := range group.Choices {
			covered[name] = true
			if !groupNames[name] && !special[name] {
				record(name, 0)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			delays, err := c.GroupDelay(group.Name, probeURL, timeout)
			// Mihomo 把自动挑选的结果缓存 10 秒，测速接口只在开始前清一次；
			// 测速期间有流量就会按旧结果重新缓存，坏节点还要再用 10 秒。
			// 测完再清一次，下一条连接立即按新结果挑节点。
			_ = c.Unfix(group.Name)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", group.Name, err))
				mu.Unlock()
				return
			}
			for name, delay := range delays {
				if !groupNames[name] {
					record(name, delay)
				}
			}
		}()
	}
	var single []string
	for _, group := range overview.Groups {
		if skipped[group.Name] {
			continue
		}
		for _, name := range group.Choices {
			if covered[name] || groupNames[name] || special[name] || (skip != nil && skip(name)) {
				continue
			}
			covered[name] = true
			single = append(single, name)
		}
	}
	limit := make(chan struct{}, 8)
	for _, name := range single {
		record(name, 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			if delay, err := c.ProxyDelay(name, probeURL, timeout); err == nil {
				record(name, delay)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 && len(results) == 0 {
		return nil, errors.Join(errs...)
	}
	return results, nil
}

// StreamLogs 订阅 Mihomo 的实时日志，每收到一条调用一次 handle，直到 ctx
// 取消或连接断开（例如 Mihomo 退出）。level 是最低日志级别，例如 info。
func (c Client) StreamLogs(ctx context.Context, level string, handle func(level, payload string)) error {
	request, err := c.request(http.MethodGet, c.BaseURL+"/logs?level="+url.QueryEscape(level), nil)
	if err != nil {
		return err
	}
	// 长连接不能用探测用的 2 秒超时，靠 ctx 结束。
	httpClient := http.Client{}
	if c.HTTP != nil {
		httpClient.Transport = c.HTTP.Transport
	}
	response, err := httpClient.Do(request.WithContext(ctx))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Controller 返回 HTTP %s", response.Status)
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var entry struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		if err := decoder.Decode(&entry); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		handle(entry.Type, entry.Payload)
	}
}

func (c Client) request(method, endpoint string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if c.Secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	return request, nil
}
