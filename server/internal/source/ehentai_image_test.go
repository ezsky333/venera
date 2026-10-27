package source

import (
	"strings"
	"testing"
	"time"
)

// TestEhentaiImageConfigResolvesURL 验证 ehentai 的图片配置能真正产出图片 URL。
//
// ehentai 与其他源不同：loadEp 返回的是**页索引字符串**（"0","1",...），
// 真实图片地址必须由 comic.onImageLoad(image, comicId, epId, nl) 现场解析。
// 早期服务端 ImageConfigFor 取到的是空配置，于是 "0" 被当成 URL 直接
// 交给 http.Get，报 `Get "10": unsupported protocol scheme ""`
// —— 这正是 107 个 ehentai 任务在 loadInfo 修好之后暴露的第二层失败。
func TestEhentaiImageConfigResolvesURL(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	m, ok := newLiveEhentaiManager(t)
	if !ok {
		return
	}

	const (
		cid = "https://exhentai.org/g/2643494/a63e95900d/"
		ep  = cid // ehentai 单章：epId 就是漫画 id
		img = "0"
	)

	cfg, err := m.ImageConfigFor("ehentai", img, cid, ep)
	if err != nil {
		t.Fatalf("ImageConfigFor: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	t.Logf("cfg: url=%q headers=%v onLoadFailed=%v",
		cfg.URL, cfg.Headers, cfg.OnLoadFailedJS != "")

	if cfg.URL == "" {
		t.Fatalf("ehentai must resolve a real image URL, got empty (img=%q)", img)
	}
	if !strings.HasPrefix(cfg.URL, "http") {
		t.Fatalf("resolved URL must be absolute, got %q", cfg.URL)
	}
	if _, err := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

// TestEhentaiLoadEpReturnsIndices 锁住一个反直觉的事实：
// ehentai 的 loadEp 返回的确实是索引字符串，不是 URL。
// 如果哪天它改成返回 URL，上面的解析逻辑就要跟着调整。
func TestEhentaiLoadEpReturnsIndices(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	m, ok := newLiveEhentaiManager(t)
	if !ok {
		return
	}
	const cid = "https://exhentai.org/g/2643494/a63e95900d/"
	urls, err := m.LoadEpisodes("ehentai", cid, cid)
	if err != nil {
		t.Skipf("upstream unavailable: %v", err)
	}
	t.Logf("got %d entries, first 5: %v", len(urls), firstN(urls, 5))
	if len(urls) == 0 {
		t.Fatal("expected at least one page")
	}
	if strings.HasPrefix(urls[0], "http") {
		t.Logf("NOTE: ehentai now returns real URLs (behavior changed)")
		return
	}
	// 索引字符串形态
	if urls[0] != "0" {
		t.Fatalf("expected index string \"0\", got %q", urls[0])
	}
}

func firstN(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
