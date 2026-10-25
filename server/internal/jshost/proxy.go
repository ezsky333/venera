package jshost

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// DefaultProxyBypassList 是默认的代理绕过列表（局域网 + 回环）。
//
// 这里保留一份副本，使 jshost 包可以独立使用（config 包会引用它，
// 避免两处默认值不一致）。
const DefaultProxyBypassList = "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,*.local"

// ProxyConfig 描述出口代理的配置。
//
// 设计目标（对应两个真实需求）：
//  1. 漫画源站点可能需要代理才能访问（例如 ehentai 在部分网络下不可达）；
//  2. **NAS 自身的流量绝不能走代理** —— 客户端访问 NAS 的是局域网地址，
//     若被代理吞掉会直接超时。服务端 API 由 net/http 直接监听，本身不受
//     此配置影响；此外我们仍然对"目标是局域网"的请求强制直连，
//     以防用户把 NAS 域名写进了代理规则。
type ProxyConfig struct {
	// Raw 是原始配置值：
	//   "" 或 "none" → 直连
	//   "env"        → 使用环境变量
	//   http(s)://…  → HTTP 代理
	//   socks5://…   → SOCKS5 代理
	Raw string
	// Bypass 是绕过代理的列表（逗号分隔，支持 IP、CIDR、域名后缀）。
	Bypass string
}

// proxyFunc 返回 http.Transport 可用的 Proxy 函数。
// 返回 nil 表示直连。
func (p ProxyConfig) proxyFunc() func(*http.Request) (*url.URL, error) {
	raw := strings.TrimSpace(p.Raw)
	bypass := parseBypass(p.Bypass)

	// 直连
	if raw == "" || strings.EqualFold(raw, "none") || strings.EqualFold(raw, "direct") {
		return nil
	}

	// 环境变量模式：尊重 HTTP_PROXY/NO_PROXY，但也叠加我们的 bypass
	if strings.EqualFold(raw, "env") {
		base := http.ProxyFromEnvironment
		return func(r *http.Request) (*url.URL, error) {
			if host := r.URL.Hostname(); shouldBypass(host, bypass) {
				return nil, nil
			}
			return base(r)
		}
	}

	// 显式代理地址
	u, err := url.Parse(ensureScheme(raw))
	if err != nil {
		return func(*http.Request) (*url.URL, error) {
			return nil, fmt.Errorf("invalid proxy %q: %w", raw, err)
		}
	}

	return func(r *http.Request) (*url.URL, error) {
		if host := r.URL.Hostname(); shouldBypass(host, bypass) {
			return nil, nil
		}
		return u, nil
	}
}

// transport 为给定配置构造 Transport。
func (p ProxyConfig) transport() *http.Transport {
	tr := &http.Transport{
		Proxy: p.proxyFunc(),
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ForceAttemptHTTP2:     false,
	}

	// SOCKS5 需要自定义 Dialer（http.Transport 的 Proxy 只支持 http/https/socks5 URL
	// 从 Go 1.9 起已内建支持 socks5，这里保持默认即可）。
	if u, err := url.Parse(ensureScheme(strings.TrimSpace(p.Raw))); err == nil &&
		strings.HasPrefix(u.Scheme, "socks5") {
		if d, derr := proxy.FromURL(u, proxy.Direct); derr == nil {
			tr.DialContext = nil
			tr.Dial = d.Dial
			tr.Proxy = nil
		}
	}
	return tr
}

const (
	dialTimeout          = 20 * 1e9
	keepAlive            = 30 * 1e9
	idleConnTimeout      = 60 * 1e9
	tlsHandshakeTimeout  = 20 * 1e9
	expectContinueTimeout = 1 * 1e9
)

func ensureScheme(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "://") {
		return s
	}
	return "http://" + s
}

// bypassRule 是一条绕过规则：可能是 CIDR、具体 IP、或以 "." 开头的域名后缀。
type bypassRule struct {
	cidr   *net.IPNet
	ip     net.IP
	suffix string
	exact  string
}

func parseBypass(s string) []bypassRule {
	var out []bypassRule
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// CIDR
		if strings.Contains(part, "/") {
			if _, n, err := net.ParseCIDR(part); err == nil {
				out = append(out, bypassRule{cidr: n})
				continue
			}
		}
		// 具体 IP
		if ip := net.ParseIP(part); ip != nil {
			out = append(out, bypassRule{ip: ip})
			continue
		}
		// 域名后缀（*.local 或 .local）
		if strings.HasPrefix(part, "*.") {
			out = append(out, bypassRule{suffix: strings.ToLower(part[1:])})
			continue
		}
		if strings.HasPrefix(part, ".") {
			out = append(out, bypassRule{suffix: strings.ToLower(part)})
			continue
		}
		out = append(out, bypassRule{exact: strings.ToLower(part)})
	}
	return out
}

func shouldBypass(host string, rules []bypassRule) bool {
	if host == "" {
		return true
	}
	lower := strings.ToLower(host)

	// 单标签主机名（如 nas、localhost）几乎一定是局域网，直接绕过
	if !strings.Contains(lower, ".") {
		return true
	}

	var ip net.IP
	if parsed := net.ParseIP(host); parsed != nil {
		ip = parsed
		// 回环与私有网段始终直连，避免把 NAS 自己送进代理
		if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() {
			return true
		}
	}

	for _, r := range rules {
		switch {
		case r.cidr != nil:
			if ip != nil && r.cidr.Contains(ip) {
				return true
			}
		case r.ip != nil:
			if ip != nil && r.ip.Equal(ip) {
				return true
			}
		case r.suffix != "":
			if strings.HasSuffix(lower, r.suffix) {
				return true
			}
		case r.exact != "":
			if lower == r.exact {
				return true
			}
		}
	}
	return false
}
