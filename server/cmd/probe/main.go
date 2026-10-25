// Command probe 是开发期的联调工具：在真实网络上跑通一个源。
//
// 用法：
//
//	go run ./cmd/probe <sourceKey> <comicId> [--download]
//
// 它会依次调用 loadInfo → loadEp → getImageLoadingConfig → 下载首图，
// 用于在部署前验证"这个源在当前环境是否可用"。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/jshost"
	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/source"
)

func main() {
	download := flag.Bool("download", false, "download the first page of the first chapter")
	srcDir := flag.String("sources", "", "directory containing .js sources (default ../venera-configs)")
	chapterIdx := flag.Int("chapter", 0, "chapter index (0-based) to probe")
	verbose := flag.Bool("list", false, "list chapters")
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		fmt.Println("usage: probe <sourceKey> <comicId> [--download]")
		os.Exit(2)
	}
	key, comicID := args[0], args[1]

	dir := *srcDir
	if dir == "" {
		dir = filepath.Join("..", "venera-configs")
	}
	if _, err := os.Stat(dir); err != nil {
		fmt.Printf("source dir not found: %s\n", dir)
		os.Exit(2)
	}

	// 构造一个临时的 data dir，只用于本次探测
	tmp, err := os.MkdirTemp("", "venera-probe-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	srcTarget := filepath.Join(tmp, "sources")
	if err := os.MkdirAll(srcTarget, 0o755); err != nil {
		panic(err)
	}

	// 把所有源脚本拷进去（Manager 会扫描目录）
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		if strings.HasPrefix(e.Name(), "_") {
			continue // _template_.js / _venera_.js 不是真实源
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(srcTarget, e.Name()), b, 0o644)
	}

	store := newMemStore()
	mgr, err := source.NewManager(source.Options{
		DataDir:  tmp,
		Store:    store,
		InitJS:   assets.InitJS,
		Logger:   logf,
		RPS:      2,
		Timeout:  120 * time.Second,
		Platform: "linux",
		Locale:   "zh_CN",
	})
	if err != nil {
		panic(err)
	}

	if !mgr.Has(key) {
		fmt.Printf("source %q not found. installed: ", key)
		for _, s := range mgr.List() {
			fmt.Printf("%s ", s.Key)
		}
		fmt.Println()
		os.Exit(1)
	}

	fmt.Printf("→ loadInfo(%s) on %s ...\n", comicID, key)
	t0 := time.Now()
	detail, err := mgr.LoadDetail(key, comicID)
	if err != nil {
		fmt.Printf("FAIL loadInfo: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK  title=%q chapters=%d grouped=%v elapsed=%s\n",
		detail.Title, len(detail.Chapters), detail.Grouped, time.Since(t0).Round(time.Millisecond))
	if *verbose {
		for i, c := range detail.Chapters {
			if i >= 12 {
				fmt.Printf("    ... (%d more)\n", len(detail.Chapters)-i)
				break
			}
			fmt.Printf("    [%d] group=%q id=%q title=%q\n", i, c.Group, c.ID, c.Title)
		}
	}

	if len(detail.Chapters) == 0 {
		fmt.Println("no chapters; nothing more to do")
		return
	}

	// 允许用 -chapter 指定章节序号（0 基），默认取第一个"看起来可用"的。
	ci := *chapterIdx
	if ci < 0 || ci >= len(detail.Chapters) {
		ci = 0
	}
	first := detail.Chapters[ci]
	fmt.Printf("→ loadEp(%s, %q) ...\n", comicID, first.ID)
	t0 = time.Now()
	urls, err := mgr.LoadEpisodes(key, comicID, first.ID)
	if err != nil {
		fmt.Printf("FAIL loadEp: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK  pages=%d elapsed=%s\n", len(urls), time.Since(t0).Round(time.Millisecond))
	if len(urls) == 0 {
		return
	}
	fmt.Printf("    first url: %s\n", truncate(urls[0], 120))

	fmt.Printf("→ getImageLoadingConfig ...\n")
	cfg, err := mgr.ImageConfigFor(key, urls[0], comicID, first.ID)
	if err != nil {
		fmt.Printf("FAIL image config: %v\n", err)
		os.Exit(1)
	}
	if cfg != nil {
		fmt.Printf("OK  url=%s method=%s headers=%d onResponse=%v modifyImage=%v\n",
			truncate(cfg.URL, 60), cfg.Method, len(cfg.Headers),
			cfg.OnResponseJS != "", cfg.ModifyImageJS != "")
	}

	if *download {
		fmt.Printf("→ downloading first page ...\n")
		target := cfg.URL
		if target == "" {
			target = urls[0]
		}
		data, err := jshost.FetchImage(nil, target, cfg.Headers)
		if err != nil {
			fmt.Printf("FAIL download: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("OK  %d bytes (%s)\n", len(data), sniffImage(data))
		out := filepath.Join(".", "probe-page-0.bin")
		_ = os.WriteFile(out, data, 0o644)
		fmt.Printf("    saved to %s\n", out)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sniffImage(b []byte) string {
	if len(b) < 12 {
		return "too-short"
	}
	switch {
	case b[0] == 0xFF && b[1] == 0xD8:
		return "jpeg"
	case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "png"
	case b[0] == 'G' && b[1] == 'I' && b[2] == 'F':
		return "gif"
	case string(b[0:4]) == "RIFF":
		return "webp"
	}
	return fmt.Sprintf("unknown(%02x%02x)", b[0], b[1])
}

func logf(level, title, content string) {
	if level == "error" || level == "warning" {
		fmt.Printf("    [%s] %s: %s\n", level, title, truncate(content, 200))
	}
}

// memStore 是探测用的内存版 SourceDataStore。
type memStore struct {
	data    map[string]map[string]any
	cookies map[string][]jshostCookie
}

type jshostCookie = struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path,omitempty"`
	Expires  *int64  `json:"expires,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
}

func newMemStore() *memStore {
	return &memStore{data: map[string]map[string]any{}}
}

func (m *memStore) GetSourceData(key string) (map[string]any, error) {
	if d, ok := m.data[key]; ok {
		return d, nil
	}
	return map[string]any{}, nil
}

func (m *memStore) MergeSourceData(key string, patch map[string]any) error {
	if m.data[key] == nil {
		m.data[key] = map[string]any{}
	}
	for k, v := range patch {
		m.data[key][k] = v
	}
	return nil
}

func (m *memStore) GetCookies(key string) ([]model.CookieRecord, error) { return nil, nil }
func (m *memStore) PutCookies(key string, c []model.CookieRecord) error { return nil }

type modelCookie = jshostCookie

var _ = json.Marshal
