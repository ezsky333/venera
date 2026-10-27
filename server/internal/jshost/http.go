package jshost

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/venera-app/venera-nas/internal/model"
)

// HTTPDoer 抽象网络层，便于测试与注入限速。
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// RateLimiter 按主机限速。
type RateLimiter interface {
	Wait(ctx context.Context, host string) error
}

// DefaultClient 返回一个与 App 行为接近的 HTTP 客户端：
// 不自动跟随重定向以外的特殊处理，关闭 HTTP/2（部分站点指纹敏感）。
//
// 代理行为由 Proxy 配置控制；默认直连。
func DefaultClient() *http.Client {
	return DefaultClientWithProxy(ProxyConfig{})
}

// DefaultClientWithProxy 按代理配置构造客户端。
func DefaultClientWithProxy(pc ProxyConfig) *http.Client {
	return &http.Client{
		Transport: pc.transport(),
		Timeout:   90 * time.Second,
		// 默认策略会把跨域重定向一路跟到底，并返回**最终页面**。
		// 对 exhentai 这种站点这会失控：
		//   /g/<id>  (未登录 → 302)
		//     → forums.e-hentai.org/remoteapi.php?ex=…
		//       → 再次 302 → …（10 次后报 "stopped after 10 redirects"）
		// 我们真正想要的是"告诉源：这个页面需要登录/不可访问"，
		// 而不是让整个请求以传输层错误结束——源脚本无从判断状态码。
		//
		// 因此跨域重定向一律停在 3xx：把 302 原样交给源，
		// 源手里的 `if (res.status !== 200) throw …` 才能给出可读的错误。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			if len(via) > 0 && !sameSite(via[0].URL, req.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// sameSite 判断两个 URL 是否属于同一站点（忽略子域差异）。
//
// 例：e-hentai.org ↔ forums.e-hentai.org 视为跨站，
// 因为前者是画廊、后者是论坛，认证语义完全不同。
func sameSite(a, b *url.URL) bool {
	if a == nil || b == nil {
		return true
	}
	return strings.EqualFold(a.Host, b.Host)
}

// jsHTTP 实现 Network.sendRequest / fetchBytes。
//
// 返回 {status, headers, body, error}。注意 App 侧在出错时也是返回结构体
// （error 字段非空），而不是抛异常——init.js 会据 error 抛错。
func (h *Host) jsHTTP(msg map[string]any) goja.Value {
	vm := h.vm
	rawURL, _ := msg["url"].(string)
	if rawURL == "" {
		return h.httpResult(0, nil, nil, "empty url")
	}
	method, _ := msg["http_method"].(string)
	if method == "" {
		method = "GET"
	}

	// 处理 //host/path 形式
	if strings.HasPrefix(rawURL, "//") {
		rawURL = "https:" + rawURL
	}
	// 相对路径（部分源基于 this.url 拼接）直接失败，避免神秘错误
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return h.httpResult(0, nil, nil, "invalid url: "+rawURL)
	}

	// 组装 body
	//
	// 关键：JS 对象（map）必须按 Content-Type 决定序列化方式。
	// 早期实现一律按 URL 表单编码，导致所有"JSON body"接口失败——
	// ehentai 的 showpage / imagedispatch 就是典型：
	// 源脚本传 {'Content-Type': 'application/json'} + 对象，
	// 服务端却发 `gid=...&imgkey=...`，上游回
	// {"error":"Invalid JSON Request"}，于是拿不到 i3/i6，
	// 源脚本 `reg[1]` 崩溃 → 101/107 个 ehentai 任务失败。
	//
	// dio 的行为是：Content-Type 含 json → jsonEncode；否则表单编码。
	contentType := ""
	if hm, ok := msg["headers"].(map[string]any); ok {
		for k, v := range hm {
			if strings.EqualFold(k, "content-type") {
				contentType = strings.ToLower(fmt.Sprint(v))
				break
			}
		}
	}

	var body io.Reader
	if d := msg["data"]; d != nil {
		switch v := d.(type) {
		case string:
			body = strings.NewReader(v)
		case goja.ArrayBuffer:
			body = bytes.NewReader(v.Bytes())
		case []byte:
			body = bytes.NewReader(v)
		default:
			if m, ok := d.(map[string]any); ok {
				if strings.Contains(contentType, "json") {
					// JSON body：直接序列化对象（不转义 HTML，与 dio 一致）
					var buf bytes.Buffer
					enc := json.NewEncoder(&buf)
					enc.SetEscapeHTML(false)
					if err := enc.Encode(m); err != nil {
						return h.httpResult(0, nil, nil, "encode json body: "+err.Error())
					}
					body = bytes.NewReader(bytes.TrimRight(buf.Bytes(), "\n"))
				} else {
					form := url.Values{}
					for k, vv := range m {
						form.Set(k, fmt.Sprint(vv))
					}
					body = strings.NewReader(form.Encode())
				}
			} else {
				body = strings.NewReader(fmt.Sprint(d))
			}
		}
	}

	// 注入 Cookie（App 用统一的 CookieJar，服务端按源隔离）
	if h.opts.Store != nil {
		if cookies, err := h.opts.Store.GetCookies(h.opts.SourceKey); err == nil && len(cookies) > 0 {
			if u, err := url.Parse(rawURL); err == nil {
				var parts []string
				now := time.Now().UnixMilli()
				for _, c := range cookies {
					if !cookieDomainMatch(c.Domain, u.Host) {
						continue
					}
					if c.Expires != nil && *c.Expires > 0 && *c.Expires < now {
						continue
					}
					parts = append(parts, c.Name+"="+c.Value)
				}
				if len(parts) > 0 {
					if msg["headers"] == nil {
						msg["headers"] = map[string]any{}
					}
					if hm, ok := msg["headers"].(map[string]any); ok {
						existing := ""
						for k, v := range hm {
							if strings.EqualFold(k, "cookie") {
								existing = fmt.Sprint(v)
								delete(hm, k)
								break
							}
						}
						joined := strings.Join(parts, "; ")
						if existing != "" {
							joined = existing + "; " + joined
						}
						hm["Cookie"] = joined
					}
				}
			}
		}
	}

	ctx := h.ctx
	if h.opts.Limiter != nil {
		if u, err := url.Parse(rawURL); err == nil {
			if err := h.opts.Limiter.Wait(ctx, u.Host); err != nil {
				return h.httpResult(0, nil, nil, err.Error())
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), rawURL, body)
	if err != nil {
		return h.httpResult(0, nil, nil, err.Error())
	}
	applyHeaders(req, msg["headers"])
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", webUA)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}

	client := h.opts.HTTPClient
	if client == nil {
		client = DefaultClient()
	}

	// App 的行为差异（非常关键）：
	//   - Network.fetchBytes / fetch() → body 是 ArrayBuffer
	//   - Network.get/post/sendRequest → body 是 string（dio plain 解码）
	// 源大量依赖 `JSON.parse(res.body)`，因此不能一律给 ArrayBuffer。
	wantBytes, _ := msg["bytes"].(bool)

	// goja 是单线程的，HTTP 必须异步，完成后再投递回事件循环
	p, resolve, _ := vm.NewPromise()
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			h.post(func() { resolve(h.httpResult(0, nil, nil, err.Error())) })
			return
		}
		defer resp.Body.Close()

		limit := h.opts.MaxResponseBytes
		reader := io.Reader(resp.Body)
		// 源普遍会显式声明 `Accept-Encoding: gzip, deflate, br, zstd`（照抄浏览器），
		// 但 Go 的 Transport 只在"自己加的"Accept-Encoding 时才自动解压。
		// 源自己加的请求头会让 resp.Body 保持压缩态，于是 JSON.parse 读到二进制。
		// 这里按 Content-Encoding 主动解压（br/zstd 没有标准库实现，交给源降级）。
		if enc := resp.Header.Get("Content-Encoding"); enc != "" {
			decoded, derr := decodeBody(enc, resp.Body)
			if derr != nil {
				h.post(func() { resolve(h.httpResult(0, nil, nil, "decompress "+enc+": "+derr.Error())) })
				return
			}
			reader = decoded
		}
		if limit > 0 {
			reader = io.LimitReader(reader, limit+1)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			h.post(func() { resolve(h.httpResult(resp.StatusCode, nil, nil, err.Error())) })
			return
		}
		tooLarge := limit > 0 && int64(len(data)) > limit
		if tooLarge {
			data = data[:limit]
		}

		headers := map[string]string{}
		for k := range resp.Header {
			headers[strings.ToLower(k)] = resp.Header.Get(k)
		}

		h.post(func() {
			// 保存 Set-Cookie（App 的 CookieJar 行为）
			h.saveResponseCookies(rawURL, resp)
			var res goja.Value
			if tooLarge {
				res = h.httpResultRaw(resp.StatusCode, headers, data, wantBytes, "response body truncated: exceeds limit")
			} else {
				res = h.httpResultRaw(resp.StatusCode, headers, data, wantBytes, "")
			}
			resolve(res)
		})
	}()

	return vm.ToValue(p)
}

// httpResult 构造 App 兼容的响应对象。
//
// wantBytes=false 时 body 是字符串（与 dio 的 ResponseType.plain 一致），
// 这决定了源能否直接 JSON.parse(res.body)。
func (h *Host) httpResult(status int, headers map[string]string, body []byte, errMsg string) goja.Value {
	return h.httpResultRaw(status, headers, body, false, errMsg)
}

func (h *Host) httpResultRaw(status int, headers map[string]string, body []byte, wantBytes bool, errMsg string) goja.Value {
	vm := h.vm
	obj := vm.NewObject()
	if errMsg != "" {
		_ = obj.Set("status", 0)
	} else {
		_ = obj.Set("status", status)
	}
	hm := vm.NewObject()
	for k, v := range headers {
		_ = hm.Set(k, v)
	}
	_ = obj.Set("headers", hm)
	if body != nil {
		if wantBytes {
			_ = obj.Set("body", vm.NewArrayBuffer(body))
		} else {
			_ = obj.Set("body", vm.ToValue(string(body)))
		}
	} else {
		_ = obj.Set("body", goja.Null())
	}
	if errMsg != "" {
		_ = obj.Set("error", errMsg)
	} else {
		_ = obj.Set("error", goja.Null())
	}
	return obj
}

func applyHeaders(req *http.Request, raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for k, v := range m {
		if v == nil {
			continue
		}
		// http_client 是 App 的内部字段，不是真实请求头
		if strings.EqualFold(k, "http_client") {
			continue
		}
		switch vv := v.(type) {
		case string:
			req.Header.Set(k, vv)
		case []any:
			for _, e := range vv {
				req.Header.Add(k, fmt.Sprint(e))
			}
		default:
			req.Header.Set(k, fmt.Sprint(vv))
		}
	}
}

// saveResponseCookies 把响应的 Set-Cookie 落库，供下次请求复用。
func (h *Host) saveResponseCookies(rawURL string, resp *http.Response) {
	if h.opts.Store == nil {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	var recs []model.CookieRecord
	for _, c := range resp.Cookies() {
		domain := c.Domain
		if domain == "" {
			domain = u.Host
		}
		var exp *int64
		if !c.Expires.IsZero() {
			v := c.Expires.UnixMilli()
			exp = &v
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		recs = append(recs, model.CookieRecord{
			Name: c.Name, Value: c.Value, Domain: domain, Path: path,
			Expires: exp, Secure: c.Secure, HTTPOnly: c.HttpOnly,
		})
	}
	if len(recs) > 0 {
		_ = h.opts.Store.PutCookies(h.opts.SourceKey, recs)
	}
}

// cookieDomainMatch 判断 Cookie 的 domain 是否适用于目标主机。
func cookieDomainMatch(cookieDomain, host string) bool {
	if cookieDomain == "" {
		return true
	}
	cd := strings.TrimPrefix(strings.ToLower(cookieDomain), ".")
	h := strings.ToLower(host)
	return h == cd || strings.HasSuffix(h, "."+cd)
}

// jsCookie 实现 Network.getCookies / setCookies / deleteCookies。
func (h *Host) jsCookie(msg map[string]any) goja.Value {
	vm := h.vm
	fn, _ := msg["function"].(string)
	rawURL, _ := msg["url"].(string)

	switch fn {
	case "get":
		var out []model.CookieRecord
		if h.opts.Store != nil {
			cookies, err := h.opts.Store.GetCookies(h.opts.SourceKey)
			if err == nil {
				var host string
				if u, err := url.Parse(rawURL); err == nil {
					host = u.Host
				}
				for _, c := range cookies {
					if host == "" || cookieDomainMatch(c.Domain, host) {
						out = append(out, c)
					}
				}
			}
		}
		arr := vm.NewArray()
		for i, c := range out {
			o := vm.NewObject()
			_ = o.Set("name", c.Name)
			_ = o.Set("value", c.Value)
			_ = o.Set("domain", c.Domain)
			_ = o.Set("path", c.Path)
			if c.Expires != nil {
				_ = o.Set("expires", *c.Expires)
			} else {
				_ = o.Set("expires", goja.Null())
			}
			_ = o.Set("secure", c.Secure)
			_ = o.Set("httpOnly", c.HTTPOnly)
			_ = o.Set("session", c.Expires == nil)
			arr.Set(fmt.Sprint(i), o)
		}
		return arr

	case "set":
		list, _ := msg["cookies"].([]any)
		var recs []model.CookieRecord
		var defaultDomain string
		if u, err := url.Parse(rawURL); err == nil {
			defaultDomain = u.Host
		}
		for _, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			c := model.CookieRecord{
				Name:   fmt.Sprint(m["name"]),
				Value:  fmt.Sprint(m["value"]),
				Domain: defaultDomain,
				Path:   "/",
			}
			if d, ok := m["domain"].(string); ok && d != "" {
				c.Domain = d
			}
			if p, ok := m["path"].(string); ok && p != "" {
				c.Path = p
			}
			recs = append(recs, c)
		}
		if h.opts.Store != nil && len(recs) > 0 {
			_ = h.opts.Store.PutCookies(h.opts.SourceKey, recs)
		}
		return goja.Undefined()

	case "delete":
		// 简化处理：清空该域下的 Cookie
		if h.opts.Store != nil {
			_ = h.opts.Store.PutCookies(h.opts.SourceKey, nil)
		}
		return goja.Undefined()
	}
	return goja.Null()
}

// webUA 与 App 的 lib/foundation/consts.dart 保持一致。
const webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"

// decodeBody 按 Content-Encoding 解压响应体。
//
// 只处理标准库能覆盖的编码。br/zstd 通常出现在源"照抄浏览器请求头"
// 却拿到压缩响应的场景；此时明确报错，比把二进制塞给 JSON.parse 更容易排查。
func decodeBody(encoding string, body io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip", "x-gzip":
		return gzip.NewReader(body)
	case "deflate":
		// 少数站点发原始 deflate 流而非 zlib 包装，先按 zlib 试，失败再裸解。
		data, err := io.ReadAll(io.LimitReader(body, 64<<20))
		if err != nil {
			return nil, err
		}
		if zr, zerr := zlib.NewReader(bytes.NewReader(data)); zerr == nil {
			return zr, nil
		}
		return flate.NewReader(bytes.NewReader(data)), nil
	case "br", "zstd":
		return nil, fmt.Errorf("unsupported encoding %q (remove it from the request's Accept-Encoding)", encoding)
	}
	return body, nil
}

var _ = sync.Once{}
