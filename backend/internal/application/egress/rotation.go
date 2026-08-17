package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"

	_ "github.com/bdandy/go-socks4"
	xproxy "golang.org/x/net/proxy"
)

// ErrRotationUnavailable 表示未配置内建 resin 轮换。
var ErrRotationUnavailable = errors.New("未配置质量守护轮换")

// ResinRotationConfig 是内建 resin 租约轮换的运行时配置。
// 平台与账号从节点代理 URL 的用户名段 (platform.account) 实时解析,
// 节点代理地址与 resinBaseURL 的主机名一致即视为 resin 托管节点。
type ResinRotationConfig struct {
	BaseURL    string
	AdminToken string
	EchoURL    string
	Timeout    time.Duration
}

// RotationResult 是质量守护轮换契约的响应体。
type RotationResult struct {
	Changed bool   `json:"changed"`
	ExitIP  string `json:"exitIp,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

const defaultRotationEchoURL = "https://1.1.1.1/cdn-cgi/trace"

var traceIPPattern = regexp.MustCompile(`(?m)^ip=([0-9a-fA-F:.]+)\s*$`)

// resinRotator 持有 resin 管理 API 客户端与平台名到 UUID 的解析缓存。
type resinRotator struct {
	cfg       ResinRotationConfig
	api       *http.Client
	echo      func(ctx context.Context, proxyURL string) (string, error)
	mu        sync.Mutex
	platforms map[string]string
}

// SetResinRotation 启用内建 resin 租约轮换; 传空配置表示关闭。
func (s *Service) SetResinRotation(cfg ResinRotationConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(cfg.BaseURL) == "" {
		s.rotation = nil
		return
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if strings.TrimSpace(cfg.EchoURL) == "" {
		cfg.EchoURL = defaultRotationEchoURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	s.rotation = &resinRotator{
		cfg: cfg,
		api: &http.Client{Timeout: cfg.Timeout},
		echo: func(echoCtx context.Context, proxyURL string) (string, error) {
			return rotationEcho(echoCtx, proxyURL, cfg.EchoURL)
		},
		platforms: map[string]string{},
	}
}

// RotateEgressLease 实现质量守护的换 IP 契约: 释放节点对应的 resin 租约,
// 经节点自身代理实测出口 IP, 确认变化后返回 Changed=true。
// 非 resin 托管的节点返回 Changed=false, 不视为错误。
//
// resin 粘性租约按账号身份 (platform.account) 锚定, 节点的代理模板通常使用
// {account} 占位符, 因此轮换必须释放该节点全部已绑定 Build 账号的租约,
// 而不是某个固定账号。
func (s *Service) RotateEgressLease(ctx context.Context, nodeID uint64, oldExitIP string) (RotationResult, error) {
	if nodeID == 0 {
		return RotationResult{}, fmt.Errorf("%w: nodeId 必填", ErrInvalidInput)
	}
	s.mu.RLock()
	rotator := s.rotation
	s.mu.RUnlock()
	if rotator == nil {
		return RotationResult{}, ErrRotationUnavailable
	}
	node, err := s.repository.GetEgressNode(ctx, nodeID)
	if errors.Is(err, repository.ErrNotFound) {
		return RotationResult{}, ErrNotFound
	}
	if err != nil {
		return RotationResult{}, err
	}
	proxyURL, err := s.cipher.Decrypt(strings.TrimSpace(node.EncryptedProxyURL))
	if err != nil {
		return RotationResult{}, fmt.Errorf("解密节点代理配置: %w", err)
	}
	platform, fixedAccount, ok := rotator.parseResinTemplate(proxyURL)
	if !ok {
		return RotationResult{Changed: false, Reason: "not_resin_backed"}, nil
	}
	identities := []string{}
	if fixedAccount != "" {
		identities = append(identities, fixedAccount)
	} else {
		identities, err = s.boundEgressIdentities(ctx, nodeID)
		if err != nil {
			return RotationResult{}, err
		}
		if len(identities) == 0 {
			return RotationResult{Changed: false, Reason: "no_bound_accounts"}, nil
		}
	}
	platformID, err := rotator.platformID(ctx, platform)
	if err != nil {
		return RotationResult{}, err
	}
	previous := map[string]bool{}
	for _, ip := range []string{oldExitIP, node.ExitIP, node.IPv4Probe.ExitIP, node.IPv6Probe.ExitIP} {
		if trimmed := strings.TrimSpace(ip); trimmed != "" {
			previous[trimmed] = true
		}
	}
	rotationCtx, cancel := context.WithTimeout(ctx, rotator.cfg.Timeout)
	defer cancel()
	var exitIP string
	for attempt := 0; attempt < 2; attempt++ {
		for _, identity := range identities {
			if err := rotator.releaseLease(rotationCtx, platformID, identity); err != nil {
				return RotationResult{}, err
			}
		}
		exitIP, err = rotator.echo(rotationCtx, rotator.renderProxyURL(proxyURL, identities[0]))
		if err != nil {
			return RotationResult{}, fmt.Errorf("轮换后出口探测失败: %w", err)
		}
		if exitIP != "" && !previous[exitIP] {
			return RotationResult{Changed: true, ExitIP: exitIP}, nil
		}
	}
	return RotationResult{Changed: false, ExitIP: exitIP, Reason: "exit_ip_unchanged"}, nil
}

// boundEgressIdentities 返回绑定到该节点的全部启用 Build 账号的出口身份。
// 身份规则与 WithCredential 一致: 弱关联账号使用 EgressIdentity, 否则回退为
// grok_build_<id>。
func (s *Service) boundEgressIdentities(ctx context.Context, nodeID uint64) ([]string, error) {
	if s.accounts == nil {
		return nil, errors.New("账号绑定仓库未装配")
	}
	credentials, err := s.accounts.ListEgressAssignments(ctx, accountdomain.ProviderBuild)
	if err != nil {
		return nil, fmt.Errorf("读取节点绑定账号: %w", err)
	}
	seen := map[string]bool{}
	identities := []string{}
	for _, credential := range credentials {
		if credential.EgressNodeID != nodeID || !credential.Enabled {
			continue
		}
		identity := strings.TrimSpace(credential.EgressIdentity)
		if identity == "" {
			identity = fmt.Sprintf("%s_%d", accountdomain.ProviderBuild, credential.ID)
		}
		if !seen[identity] {
			seen[identity] = true
			identities = append(identities, identity)
		}
	}
	return identities, nil
}

// parseResinURL 解析代理 URL; 存量数据可能保留未编码的 {account} 占位符,
// 统一先编码再解析。
func parseResinURL(proxyURL string) (*url.URL, error) {
	cleaned := strings.NewReplacer("{account}", "%7Baccount%7D", "%7baccount%7d", "%7Baccount%7D").Replace(strings.TrimSpace(proxyURL))
	return url.Parse(cleaned)
}

// parseResinTemplate 判定代理 URL 是否指向配置的 resin 实例, 并从用户名段
// 拆出平台名与固定账号; 使用 {account} 占位符时固定账号为空。
// 账号本身可能包含点号, 平台名按第一个点切分。
func (r *resinRotator) parseResinTemplate(proxyURL string) (string, string, bool) {
	parsed, err := parseResinURL(proxyURL)
	if err != nil || parsed.User == nil {
		return "", "", false
	}
	base, err := url.Parse(r.cfg.BaseURL)
	if err != nil {
		return "", "", false
	}
	if !hostsEqual(parsed, base) {
		return "", "", false
	}
	username := parsed.User.Username()
	platform, account, found := strings.Cut(username, ".")
	if !found || platform == "" {
		return "", "", false
	}
	if account == "" || account == ProxyAccountPlaceholder {
		return platform, "", true
	}
	return platform, account, true
}

// renderProxyURL 把代理模板中的 {account} 占位符替换为指定身份。
func (r *resinRotator) renderProxyURL(template, identity string) string {
	parsed, err := parseResinURL(template)
	if err != nil || parsed.User == nil {
		return template
	}
	username := parsed.User.Username()
	if !strings.Contains(username, ProxyAccountPlaceholder) {
		return template
	}
	rendered := strings.ReplaceAll(username, ProxyAccountPlaceholder, identity)
	password, _ := parsed.User.Password()
	parsed.User = url.UserPassword(rendered, password)
	return parsed.String()
}

func hostsEqual(proxy, base *url.URL) bool {
	proxyHost, baseHost := proxy.Hostname(), base.Hostname()
	if !strings.EqualFold(proxyHost, baseHost) {
		return false
	}
	proxyPort, basePort := proxy.Port(), base.Port()
	if basePort == "" {
		basePort = defaultPort(base.Scheme)
	}
	if proxyPort == "" {
		proxyPort = defaultPort(proxy.Scheme)
	}
	return proxyPort == basePort
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http", "socks4", "socks4a", "socks5", "socks5h":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// platformID 解析并缓存平台名对应的 resin UUID。
func (r *resinRotator) platformID(ctx context.Context, name string) (string, error) {
	r.mu.Lock()
	cached := r.platforms[name]
	r.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	var payload struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := r.apiRequest(ctx, http.MethodGet, "/api/v1/platforms", &payload); err != nil {
		return "", fmt.Errorf("列出 resin 平台: %w", err)
	}
	r.mu.Lock()
	for _, item := range payload.Items {
		r.platforms[item.Name] = item.ID
	}
	resolved := r.platforms[name]
	r.mu.Unlock()
	if resolved == "" {
		return "", fmt.Errorf("resin 平台 %q 不存在", name)
	}
	return resolved, nil
}

// releaseLease 删除指定账号的租约; 租约不存在 (404) 视为已释放。
func (r *resinRotator) releaseLease(ctx context.Context, platformID, account string) error {
	err := r.apiRequest(ctx, http.MethodDelete, "/api/v1/platforms/"+platformID+"/leases/"+url.PathEscape(account), nil)
	var apiErr *resinAPIError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return nil
	}
	return err
}

type resinAPIError struct {
	status int
	body   string
}

func (e *resinAPIError) Error() string { return fmt.Sprintf("resin API 返回 %d", e.status) }

// apiRequest 调用 resin 管理 API; result 为 nil 时忽略响应体。
func (r *resinRotator) apiRequest(ctx context.Context, method, path string, result any) error {
	request, err := http.NewRequestWithContext(ctx, method, r.cfg.BaseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.cfg.AdminToken)
	request.Header.Set("Accept", "application/json")
	response, err := r.api.Do(request)
	if err != nil {
		return fmt.Errorf("resin API 请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &resinAPIError{status: response.StatusCode, body: string(body)}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("解析 resin API 响应: %w", err)
	}
	return nil
}

// rotationEcho 经节点代理请求 echo 端点并解析出口 IP。
// 支持 Cloudflare trace 文本 (ip=...) 与 {"ip": "..."} JSON 两种格式。
func rotationEcho(ctx context.Context, proxyURL, echoURL string) (string, error) {
	client, err := rotationProxyClient(proxyURL, 15*time.Second)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, echoURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("echo 端点返回 %d", response.StatusCode)
	}
	return parseEchoIP(body), nil
}

// rotationProxyClient 为出口探测构造经指定代理的 HTTP client。
// 与 infra/egress 的 Build client 同族, 在此复制以避免 application 层与
// infra/egress 之间的循环依赖。
func rotationProxyClient(proxyURL string, responseHeaderTimeout time.Duration) (*http.Client, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("解析出口代理: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	case "socks4", "socks4a", "socks5", "socks5h":
		dialer, err := xproxy.FromURL(parsed, direct)
		if err != nil {
			return nil, fmt.Errorf("创建 SOCKS 代理: %w", err)
		}
		transport.DialContext = rotationDialContext(dialer)
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q", parsed.Scheme)
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// rotationDialContext 兼容不实现 ContextDialer 的 SOCKS 拨号器。
func rotationDialContext(dialer xproxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextual, ok := dialer.(xproxy.ContextDialer); ok {
		return contextual.DialContext
	}
	type result struct {
		connection net.Conn
		err        error
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		completed := make(chan result, 1)
		go func() {
			connection, err := dialer.Dial(network, address)
			completed <- result{connection: connection, err: err}
		}()
		select {
		case value := <-completed:
			return value.connection, value.err
		case <-ctx.Done():
			go func() {
				value := <-completed
				if value.connection != nil {
					value.connection.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}
}

func parseEchoIP(body []byte) string {
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if ip := net.ParseIP(strings.TrimSpace(payload.IP)); ip != nil {
			return ip.String()
		}
	}
	if match := traceIPPattern.FindSubmatch(body); match != nil {
		if ip := net.ParseIP(string(match[1])); ip != nil {
			return ip.String()
		}
	}
	return ""
}
