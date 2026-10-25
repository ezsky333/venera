package jshost

import (
	"net/http"
	"testing"
)

// 这组测试守护两个真实需求：
//  1. 漫画源站点可以走代理；
//  2. NAS / 局域网目标绝不能走代理（否则客户端连不上服务端）。

func proxyFor(t *testing.T, cfg ProxyConfig, rawURL string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("bad url %s: %v", rawURL, err)
	}
	fn := cfg.proxyFunc()
	if fn == nil {
		return ""
	}
	u, err := fn(req)
	if err != nil {
		t.Fatalf("proxyFunc error: %v", err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

func TestProxyEmptyMeansDirect(t *testing.T) {
	if got := proxyFor(t, ProxyConfig{}, "https://e-hentai.org/g/1/"); got != "" {
		t.Fatalf("empty proxy should be direct, got %q", got)
	}
	for _, v := range []string{"none", "direct", "NONE"} {
		if got := proxyFor(t, ProxyConfig{Raw: v}, "https://example.com/"); got != "" {
			t.Fatalf("%q should be direct, got %q", v, got)
		}
	}
}

func TestProxyAppliesToPublicHost(t *testing.T) {
	cfg := ProxyConfig{Raw: "http://127.0.0.1:7890", Bypass: DefaultProxyBypassList}
	got := proxyFor(t, cfg, "https://e-hentai.org/g/1/")
	if got != "http://127.0.0.1:7890" {
		t.Fatalf("public host should use proxy, got %q", got)
	}
}

func TestProxyBypassesLAN(t *testing.T) {
	cfg := ProxyConfig{Raw: "http://127.0.0.1:7890", Bypass: DefaultProxyBypassList}
	cases := []string{
		"http://192.168.123.212:18080/api/v1/ping",
		"http://10.0.0.5/x",
		"http://172.16.3.4/x",
		"http://127.0.0.1:8080/x",
		"http://localhost:8080/x",
		"http://nas:8080/x",
		"http://mynas.local/x",
	}
	for _, u := range cases {
		if got := proxyFor(t, cfg, u); got != "" {
			t.Errorf("%s should bypass proxy, got %q", u, got)
		}
	}
}

func TestProxyBypassCustomRules(t *testing.T) {
	cfg := ProxyConfig{
		Raw:    "http://proxy:3128",
		Bypass: "example.com,socks-target.org,.internal",
	}
	if got := proxyFor(t, cfg, "https://example.com/a"); got != "" {
		t.Fatalf("exact rule failed: %q", got)
	}
	if got := proxyFor(t, cfg, "https://api.internal/a"); got != "" {
		t.Fatalf("suffix rule failed: %q", got)
	}
	if got := proxyFor(t, cfg, "https://other.com/a"); got == "" {
		t.Fatal("non-bypassed host should use proxy")
	}
}

func TestProxySocks5(t *testing.T) {
	cfg := ProxyConfig{Raw: "socks5://127.0.0.1:1080"}
	tr := cfg.transport()
	if tr == nil {
		t.Fatal("transport is nil")
	}
	if tr.Dial == nil {
		t.Fatal("socks5 should install a custom Dial")
	}
}

func TestBypassNeverSendsLoopbackToProxy(t *testing.T) {
	cfg := ProxyConfig{Raw: "http://proxy:3128", Bypass: ""}
	for _, u := range []string{
		"http://127.0.0.1/x",
		"http://192.168.1.1/x",
		"http://10.1.2.3/x",
		"http://172.20.1.1/x",
	} {
		if got := proxyFor(t, cfg, u); got != "" {
			t.Errorf("%s must stay direct, got %q", u, got)
		}
	}
}

func TestParseBypass(t *testing.T) {
	rules := parseBypass("127.0.0.1,10.0.0.0/8,*.local,.corp,host.example")
	if len(rules) != 5 {
		t.Fatalf("expected 5 rules, got %d", len(rules))
	}
	if !shouldBypass("anything.local", rules) {
		t.Fatal("*.local rule failed")
	}
	if !shouldBypass("x.corp", rules) {
		t.Fatal(".corp rule failed")
	}
	if shouldBypass("public.example.org", rules) {
		t.Fatal("public host should not bypass")
	}
}

func TestEnsureScheme(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:7890":  "http://127.0.0.1:7890",
		"http://a:1":      "http://a:1",
		"socks5://a:1080": "socks5://a:1080",
		"":                "",
	}
	for in, want := range cases {
		if got := ensureScheme(in); got != want {
			t.Errorf("ensureScheme(%q) = %q, want %q", in, got, want)
		}
	}
}
