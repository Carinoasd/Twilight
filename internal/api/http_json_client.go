package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// sharedHTTPTransport 为所有外部 HTTP 调用（emby / telegram / 系统更新自检）
// 共享连接池，避免之前每次调用都 `&http.Client{}` 导致 TCP / TLS 握手不复用、
// 文件描述符累积、GC 压力上升等问题。
// 注意：超时不在 client 上设置，而是通过 context.WithTimeout 在每次调用时
// 控制，这样可以实现"每端点不同超时"（health 1.5s / userOp 5s / admin 10s）
// 又复用同一个 Transport。
var sharedHTTPTransport = &http.Transport{
	Proxy: guardedEnvironmentProxy,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		// 拨号阶段拦截 SSRF：Control 拿到的是**解析之后**的 IP:port，因此配置里
		// 写域名（URL 层校验管不到）以及 DNS rebinding 都能挡住。见
		// outbound_url.go::guardOutboundDialAddress。
		Control: func(network, address string, _ syscall.RawConn) error {
			return guardOutboundDialAddress(network, address)
		},
	}).DialContext,
	ForceAttemptHTTP2:      true,
	MaxIdleConns:           64,
	MaxIdleConnsPerHost:    8,
	IdleConnTimeout:        45 * time.Second,
	TLSHandshakeTimeout:    10 * time.Second,
	ExpectContinueTimeout:  1 * time.Second,
	MaxResponseHeaderBytes: 1 << 20,
}

// sharedHTTPProxyFromEnvironment 是代理来源，变量形式便于测试替换
// （http.ProxyFromEnvironment 只在进程内读一次环境变量）。
var sharedHTTPProxyFromEnvironment = http.ProxyFromEnvironment

// guardedEnvironmentProxy 修复：设置了 HTTP(S)_PROXY 时，拨号阶段的 Control 只能
// 看到代理本身的 IP，guardOutboundDialAddress 对真正的目标主机失效。这里在决定
// 走代理的那一刻改为校验目标主机：字面 IP 直接判，域名先在本机解析再逐个判，
// 命中链路本地 / 云元数据 / 未指定地址就拒绝发出请求。
//
// 本机解析失败时放行（记录由上层错误体现）：纯代理环境里本机可能没有可用 DNS，
// 此时拒绝会让 Telegram / Emby 全部不可用；剩余风险是代理端解析结果与本机不同
// （DNS rebinding 经代理），这部分只能靠代理自身的出站策略兜底。
// 开发者 JS 的 fetch 不经过这里：它使用独立 Transport 且 Proxy=nil，拨号层始终
// 能看到真实目标 IP。
func guardedEnvironmentProxy(req *http.Request) (*url.URL, error) {
	proxyURL, err := sharedHTTPProxyFromEnvironment(req)
	if err != nil || proxyURL == nil || req == nil || req.URL == nil {
		return proxyURL, err
	}
	if err := guardOutboundProxyTarget(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	return proxyURL, nil
}

func guardOutboundProxyTarget(ctx context.Context, host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("出站请求缺少目标主机")
	}
	if ip := net.ParseIP(host); ip != nil {
		return refuseUnsafeOutboundIP(ip, "出站代理目标")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if err := refuseUnsafeOutboundIP(addr.IP, "出站代理目标"); err != nil {
			return err
		}
	}
	return nil
}

// sharedHTTPClient 是 transport-only 共享 client，不带 client.Timeout，
// 所有超时都通过传入的 ctx 控制（context.DeadlineExceeded 优雅取消，
// 而 client.Timeout 触发的是连接强杀，不利于排查）。
//
// CheckRedirect 显式拒绝跨主机重定向。Go 默认策略只剥离 Authorization /
// Cookie / WWW-Authenticate 三个头，**不会**剥离自定义头；而本 client
// 的所有调用方（Emby `X-Emby-Token`、Telegram bot token in URL、Bangumi
// `Authorization: Bearer`、TMDB `api_key=` 查询串）都会把可信凭据带在
// 跨主机依然保留的位置。一个被入侵 / 错配的上游 302 到攻击者控制的 host
// 就足以把 token 原样泄露。当前所有调用方都是单次 JSON 请求，没有任何
// 路径需要跨主机 follow；同主机（scheme + host:port 完全一致）保留最多
// 5 跳以应对 trailing-slash / index 这类合理重定向。
var sharedHTTPClient = &http.Client{
	Transport:     sharedHTTPTransport,
	CheckRedirect: sameHostRedirectPolicy,
}

// sameHostRedirectPolicy 只允许同 scheme + 同 host:port 的重定向，且最多 5 跳。
// 跨主机直接返回 ErrUseLastResponse —— 调用方拿到的是原始 302 响应（结合
// `resp.StatusCode >= 400` 检查会被当作非预期状态返回错误，不会出现 token
// 已被发到第三方的窗口）。
func sameHostRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return http.ErrUseLastResponse
	}
	if len(via) == 0 {
		return nil
	}
	prev := via[len(via)-1].URL
	next := req.URL
	if prev == nil || next == nil {
		return http.ErrUseLastResponse
	}
	if !strings.EqualFold(prev.Scheme, next.Scheme) || !strings.EqualFold(prev.Host, next.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}

func getJSON(ctx context.Context, endpoint string, headers map[string]string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return doJSONRequest(req, dst)
}

func postJSON(ctx context.Context, endpoint string, headers map[string]string, body any, dst any) error {
	return postJSONWithTimeout(ctx, endpoint, headers, body, dst, 10*time.Second)
}

func postJSONWithTimeout(ctx context.Context, endpoint string, headers map[string]string, body any, dst any, timeout time.Duration) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return doJSONRequestWithTimeout(req, dst, timeout)
}

func patchJSON(ctx context.Context, endpoint string, headers map[string]string, body any, dst any) error {
	return patchJSONWithTimeout(ctx, endpoint, headers, body, dst, 10*time.Second)
}

func patchJSONWithTimeout(ctx context.Context, endpoint string, headers map[string]string, body any, dst any, timeout time.Duration) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return doJSONRequestWithTimeout(req, dst, timeout)
}

func doJSONRequest(req *http.Request, dst any) error {
	return doJSONRequestWithTimeout(req, dst, 10*time.Second)
}

// doJSONRequestWithTimeout 把 timeout 包成 context deadline 后用共享 client 发送，
// 既能复用连接池又能保留每端点不同的超时语义。
// 边界：req 已经携带 ctx（NewRequestWithContext），如果调用方 ctx 已带 deadline
// 且早于 timeout，会沿用调用方的 ctx；否则 wrap 一层确保有上界。
func doJSONRequestWithTimeout(req *http.Request, dst any, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	parentCtx := req.Context()
	if _, ok := parentCtx.Deadline(); !ok {
		ctx, cancel := context.WithTimeout(parentCtx, timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		detail := strings.TrimSpace(string(data))
		if detail != "" {
			return fmt.Errorf("remote status %d: %s", resp.StatusCode, truncateString(detail, 300))
		}
		return fmt.Errorf("remote status %d", resp.StatusCode)
	}
	// 3xx 仅会出现在 sameHostRedirectPolicy 拒绝的跨主机 / 超跳数场景：
	// Client 拿到原始 3xx 响应，不再继续 follow。把 Location 当作错误抛出，
	// 既能让调用方明确感知"被禁的重定向"，又不会因为 json.Unmarshal HTML/空 body
	// 报出难以定位的错。Token 在这一刻还没被发到第三方。
	if resp.StatusCode >= 300 {
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		if loc == "" {
			return fmt.Errorf("remote status %d: cross-host or excessive redirect refused", resp.StatusCode)
		}
		return fmt.Errorf("remote status %d: cross-host redirect refused (Location=%s)", resp.StatusCode, truncateString(loc, 200))
	}
	if dst == nil {
		return nil
	}
	// 成功状态 + 空 body（典型：Emby 的 /Users/{id}/Policy、/Sessions/{id}/Logout 等
	// 写操作返回 204 No Content；部分 Telegram setX 也只回空体）此时 dst 保持零值、
	// 按成功处理。否则 json.Unmarshal 一个空串会抛 "unexpected end of JSON input"，
	// 把本已成功的远端写操作误判为失败——这正是「禁用 Emby 提示 unexpected end of
	// JSON input」的根因。
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}

func InitHTTPClients() {
	sharedHTTPTransport.CloseIdleConnections()
}
