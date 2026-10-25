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
		client = DefaultClient()
	}
	return fetchImageWith(client, rawURL, headers)
}

// FetchImageWithProxy 按代理配置下载图片。
func FetchImageWithProxy(client HTTPDoer, rawURL string, headers map[string]string) ([]byte, error) {
	return FetchImage(client, rawURL, headers)
}

func fetchImageWith(client HTTPDoer, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
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
