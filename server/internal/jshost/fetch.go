package jshost

import (
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"
)

// FetchImage 按给定 headers 下载一张图片，返回原始字节。
//
// 注意：这里只做传输，不处理源要求的 onResponse/modifyImage 变换。
// 变换由调用方拿到字节后按 ImageConfig 执行（见 TransformOnResponse /
// TransformModifyImage）。
func FetchImage(client HTTPDoer, rawURL string, headers map[string]string) ([]byte, error) {
	if client == nil {
		client = imageClient(ProxyConfig{})
	}
	return fetchImageWith(client, rawURL, headers)
}

// FetchImageWithProxy 按代理配置下载图片。
//
// 使用 imageClient（跟随重定向）而不是 API 用的 DefaultClient：
// 图床 301 跳 CDN 是常态，见 imageClient 的注释。
func FetchImageWithProxy(_ HTTPDoer, rawURL string, headers map[string]string) ([]byte, error) {
	return fetchImageWith(imageClient(proxyProvider()), rawURL, headers)
}

// proxyProvider 返回进程级代理配置（由 main 通过 SetProxyProvider 注册）。
var proxyProvider = func() ProxyConfig { return ProxyConfig{} }

// SetProxyProvider 注册进程级代理配置读取器（main 启动时调用一次）。
func SetProxyProvider(fn func() ProxyConfig) { proxyProvider = fn }

func fetchImageWith(client HTTPDoer, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		// 关键：Accept-Encoding 必须剥离。Go 的 http.Transport 只在
		// **自己**添加该头时才自动解压 gzip；调用方手动设置会关闭
		// 自动解压，CDN 返回的压缩流被原样交给图片解码器
		// （实测 jm：全章 45/45 "decode image: unknown format"）。
		// App 的 dio 会自动解压，这里交给 transport 等价处理。
		if strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", webUA)
	}
	// 很多图床校验 Referer；未指定时用自身 origin 兜底，这通常是可接受的默认。
	if req.Header.Get("Referer") == "" {
		if o := originOf(rawURL); o != "" {
			req.Header.Set("Referer", o+"/")
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("image fetch: status %d for %s", resp.StatusCode, truncateURL(rawURL))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("image fetch: empty body")
	}
	return b, nil
}

// imageClient 返回**专用于图片下载**的 HTTP 客户端。
//
// 与 API 用的 DefaultClient 的唯一区别是重定向策略：
// API 调用把 3xx 原样交回源脚本（exhentai 用 302 表达"需要登录"，
// 源脚本靠 `if (res.status !== 200)` 给出可读错误）；而图片请求没有
// 这个语义，图床把请求 301/302 跳到 CDN 是**常态**——
// picacg 的封面（storage1.picacomic.com → storage-b.picacomic.com）
// 曾因跨 host 重定向被拦，导致全库 1 万多张内页下载成功、封面却
// 只落盘了 1 张。因此图片客户端必须跟随重定向。
//
// 返回的 client 无共享状态，调用方可按需缓存。
func imageClient(pc ProxyConfig) *http.Client {
	return &http.Client{
		Transport: pc.transport(),
		Timeout:   90 * time.Second,
	}
}

func originOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i <= 0 {
		return ""
	}
	scheme := rawURL[:i]
	rest := rawURL[i+3:]
	host := rest
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		host = rest[:j]
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func truncateURL(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

// transformHost 创建一个只用于一次性变换的极简 Host。
func transformHost(initJS, sourceJS string, timeout time.Duration) (*Host, error) {
	if sourceJS == "" {
		sourceJS = "class _Transform extends ComicSource { key = '_transform'; }"
	}
	h, err := New(Options{
		SourceKey: "_transform",
		InitJS:    initJS,
		SourceJS:  sourceJS,
		Timeout:   timeout,
	})
	if err != nil {
		return nil, err
	}
	go h.RunLoop()
	return h, nil
}

// TransformOnResponse 在 JS 沙箱里执行源的 onResponse 函数，做字节级变换。
//
// App 的实现（lib/network/images.dart）把 Uint8List 传给 JS 函数，
// 函数返回新的 ArrayBuffer。这里用一次性 VM 复刻，避免污染主 VM。
//
// funcSource 可以是函数源码（"function onResponse(buf){...}" 或箭头函数），
// 也可以是表达式（如 "onResponse"），后者在 init.js 上下文里可能不存在，
// 因此统一在求值后再取函数。
func TransformOnResponse(initJS, funcSource string, data []byte) ([]byte, error) {
	h, err := transformHost(initJS, "", 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	// 定义函数并调用（funcSource 来自源的 toString()，是合法表达式）
	setup := fmt.Sprintf(`globalThis.__fn = (%s); globalThis.__in = null;`, funcSource)
	if _, err := h.Call(setup); err != nil {
		return nil, fmt.Errorf("onResponse setup: %w", err)
	}
	h.vm.Set("__in", h.vm.NewArrayBuffer(data))

	out, err := h.Call(`(() => { const r = globalThis.__fn(globalThis.__in); return r; })()`)
	if err != nil {
		return nil, err
	}
	if ab, ok := out.Export().(gojaArrayBuffer); ok {
		return ab.Bytes(), nil
	}
	return nil, fmt.Errorf("onResponse returned unexpected type %T", out.Export())
}

// TransformModifyImage 执行源的 modifyImage 脚本，做图片级变换。
//
// App 的语义（lib/utils/image.dart）：脚本内定义 modifyImage(image)，
// image 支持 copyRange / fillImageRangeAt / copyAndRotate90，
// 返回值编码为 PNG。这里复用 jsImage 的实现并调用同一套 API。
//
// 注意：脚本里常用 Image.empty() 等 API，这些由 init.js 定义，
// 因此必须传入完整的 initJS（传空会得到 "ComicSource is not defined"）。
func TransformModifyImage(initJS, script string, data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytesReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	h, err := transformHost(initJS, "", 120*time.Second)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	h.helpers.imgMu.Lock()
	h.helpers.imgSeq++
	id := h.helpers.imgSeq
	h.helpers.imgs.Store(id, img)
	h.helpers.imgMu.Unlock()

	expr := fmt.Sprintf(`(() => {
		%s
		const image = new Image(%d);
		const out = modifyImage(image);
		return out && out.key !== undefined ? out.key : -1;
	})()`, script, id)
	v, err := h.Call(expr)
	if err != nil {
		return nil, err
	}
	newID := toInt(v.Export())
	stored, ok := h.helpers.imgs.Load(newID)
	if !ok {
		return nil, fmt.Errorf("modifyImage produced no image")
	}
	outImg, ok := stored.(image.Image)
	if !ok {
		return nil, fmt.Errorf("modifyImage result is not an image")
	}
	return encodePNG(outImg)
}

// 内部小工具，避免为两个调用引入额外的 import 别名。
type gojaArrayBuffer interface{ Bytes() []byte }
