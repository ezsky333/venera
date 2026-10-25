// Command mocktest 启动一个本地 mock 服务 + 一个自定义测试源，
// 端到端验证：JS 源执行 → 图片下载 → 落盘 → 库查询。
//
// 它不依赖外网，可在 CI 中稳定运行，用于守护核心链路。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/jobs"
	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/source"
	"github.com/venera-app/venera-nas/internal/store"
)

// 这是一个遵循 ComicSource 契约的最小 venera 源。
// 它从本地 mock 服务取数据，因此可离线运行。
const testSource = `
class MockSource extends ComicSource {
  name = "Mock"
  key = "mock"
  version = "1.0.0"
  url = "__BASE__"

  comic = {
    loadInfo: async (id) => {
      const res = await Network.get(this.url + "/api/comic/" + id);
      const d = JSON.parse(res.body);
      // 故意返回 JS Map + 干扰属性，验证服务端能正确处理
      const chapters = new Map();
      for (const c of d.chapters) {
        chapters.set(c.id, c.title);
      }
      chapters.latestChapterMarker = "v2";
      return new ComicDetails({
        title: d.title,
        cover: d.cover,
        tags: {"作者": [d.author]},
        description: "mock comic",
        chapters: chapters,
      });
    },
    loadEp: async (comicId, epId) => {
      if (!epId) throw new Error("no chapter");
      const res = await Network.get(this.url + "/api/chapter/" + epId);
      const d = JSON.parse(res.body);
      return { images: d.images };
    },
  }

  getImageLoadingConfig = (url, cid, eid) => {
    return { headers: { "Mock-Auth": "yes" } };
  }
}
`

const pageCount = 5

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	base := "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/comic/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"title":"Mock Comic","author":"Tester","cover":"%s/img/cover.png","chapters":[{"id":"c1","title":"Chapter 1"},{"id":"c2","title":"Chapter 2"}]}`, base)
	})
	chapter := func(prefix string, n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var imgs []string
			for i := 0; i < n; i++ {
				imgs = append(imgs, fmt.Sprintf("%s/img/%s-%d.png", base, prefix, i))
			}
			b, _ := json.Marshal(map[string]any{"images": imgs})
			w.Write(b)
		}
	}
	mux.HandleFunc("/api/chapter/c1", chapter("c1", pageCount))
	mux.HandleFunc("/api/chapter/c2", chapter("c2", 3))
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		// 验证源提供的 header 真的被带上
		if r.Header.Get("Mock-Auth") != "yes" {
			http.Error(w, "missing Mock-Auth", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(makePNG(120, 180, color.RGBA{R: 0x33, G: 0x99, B: 0xCC, A: 0xFF}))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	time.Sleep(50 * time.Millisecond)

	tmp, err := os.MkdirTemp("", "venera-mock-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	dataDir := filepath.Join(tmp, "data")
	libDir := filepath.Join(tmp, "library")
	if err := os.MkdirAll(filepath.Join(dataDir, "sources"), 0o755); err != nil {
		log.Fatal(err)
	}
	js := string(bytes.ReplaceAll([]byte(testSource), []byte("__BASE__"), []byte(base)))
	if err := os.WriteFile(filepath.Join(dataDir, "sources", "mock.js"), []byte(js), 0o644); err != nil {
		log.Fatal(err)
	}

	st, err := store.Open(dataDir, libDir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := st.EnsureDirs(); err != nil {
		log.Fatal(err)
	}

	mgr, err := source.NewManager(source.Options{
		DataDir: dataDir,
		Store:   &storeAdapter{st},
		InitJS:  assets.InitJS,
		Logger:  func(l, t, c string) { fmt.Printf("      [%s] %s: %s\n", l, t, c) },
		RPS:     0,
		Timeout: 60 * time.Second,
		Locale:  "zh_CN",
	})
	if err != nil {
		log.Fatal(err)
	}
	if !mgr.Has("mock") {
		log.Fatalf("mock source not installed; have %+v", mgr.List())
	}

	must := func(cond bool, format string, args ...any) {
		if !cond {
			log.Fatalf("ASSERT FAILED: "+format, args...)
		}
	}

	fmt.Println("=== 1. loadInfo (JS Map chapters + noise property) ===")
	detail, err := mgr.LoadDetail("mock", "1")
	if err != nil {
		log.Fatalf("loadInfo: %v", err)
	}
	fmt.Printf("    title=%q chapters=%d grouped=%v tags=%v\n",
		detail.Title, len(detail.Chapters), detail.Grouped, detail.Tags)
	must(len(detail.Chapters) == 2, "expected 2 chapters, got %d", len(detail.Chapters))
	must(detail.Chapters[0].ID == "c1" && detail.Chapters[1].ID == "c2",
		"chapter order wrong: %+v", detail.Chapters)
	must(detail.Title == "Mock Comic", "title wrong: %q", detail.Title)

	fmt.Println("=== 2. loadEp ===")
	urls, err := mgr.LoadEpisodes("mock", "1", "c1")
	if err != nil {
		log.Fatalf("loadEp: %v", err)
	}
	fmt.Printf("    pages=%d\n", len(urls))
	must(len(urls) == pageCount, "expected %d pages, got %d", pageCount, len(urls))

	fmt.Println("=== 3. getImageLoadingConfig ===")
	cfg, err := mgr.ImageConfigFor("mock", urls[0], "1", "c1")
	if err != nil {
		log.Fatalf("image config: %v", err)
	}
	fmt.Printf("    headers=%v\n", cfg.Headers)
	must(cfg.Headers["Mock-Auth"] == "yes", "expected Mock-Auth header, got %v", cfg.Headers)

	fmt.Println("=== 4. download (headers must be forwarded) ===")
	runner := &runnerAdapter{mgr}
	dler := jobs.NewDownloader(st, runner, 3)
	dler.SetLogger(func(f string, a ...any) { fmt.Printf("      "+f+"\n", a...) })
	dler.SetFetch(func(u string, h map[string]string) ([]byte, error) {
		return fetchWithHeaders(u, h)
	})

	jobID, err := st.CreateJob(&model.Job{Source: "mock", ComicID: "1", Title: "Mock Comic", Origin: "test"})
	if err != nil {
		log.Fatal(err)
	}
	if err := dler.RunJob(jobID, detail); err != nil {
		log.Fatalf("download job: %v", err)
	}

	fmt.Println("=== 5. verify library index ===")
	cached, err := st.ListCachedByComic("mock", "1")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("    cached chapters=%d\n", len(cached))
	must(len(cached) == 2, "expected 2 cached chapters, got %d", len(cached))
	for _, c := range cached {
		fmt.Printf("    %s: pages=%d dir=%s bytes=%d\n", c.ChapterID, c.PageCount, c.Dir, c.Bytes)
		dir := filepath.Join(libDir, filepath.FromSlash(c.Dir))
		entries, err := os.ReadDir(dir)
		must(err == nil, "read %s: %v", dir, err)
		must(len(entries) == c.PageCount, "disk pages=%d db pages=%d", len(entries), c.PageCount)
		// 校验每个文件都是合法 PNG
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			must(err == nil, "read page: %v", err)
			must(isPNG(b), "page %s is not PNG (%d bytes)", e.Name(), len(b))
		}
	}

	fmt.Println("=== 6. verify cover saved ===")
	comicDir := jobs.ComicDirName("Mock Comic", "1")
	coverPath := filepath.Join(libDir, comicDir, "cover.png")
	if _, err := os.Stat(coverPath); err != nil {
		fmt.Printf("    (cover not at %s: %v)\n", coverPath, err)
	} else {
		fmt.Printf("    cover OK: %s\n", coverPath)
	}

	fmt.Println("\n✅ ALL CHECKS PASSED")
}

type storeAdapter struct{ *store.Store }

func (s *storeAdapter) GetSourceData(k string) (map[string]any, error) { return s.Store.GetSourceData(k) }
func (s *storeAdapter) MergeSourceData(k string, p map[string]any) error {
	return s.Store.MergeSourceData(k, p)
}
func (s *storeAdapter) GetCookies(k string) ([]model.CookieRecord, error) {
	return s.Store.GetCookies(k)
}
func (s *storeAdapter) PutCookies(k string, c []model.CookieRecord) error {
	return s.Store.PutCookies(k, c)
}

type runnerAdapter struct{ m *source.Manager }

func (r *runnerAdapter) LoadDetail(s, c string) (*model.ComicDetail, error) {
	return r.m.LoadDetail(s, c)
}
func (r *runnerAdapter) LoadEpisodes(s, c, e string) ([]string, error) {
	return r.m.LoadEpisodes(s, c, e)
}
func (r *runnerAdapter) ImageConfigFor(s, u, c, e string) (*jobs.ImageConfig, error) {
	cfg, err := r.m.ImageConfigFor(s, u, c, e)
	if err != nil || cfg == nil {
		return nil, err
	}
	return &jobs.ImageConfig{
		URL: cfg.URL, Method: cfg.Method, Data: cfg.Data, Headers: cfg.Headers,
		OnResponseJS: cfg.OnResponseJS, ModifyImageJS: cfg.ModifyImageJS,
	}, nil
}

func fetchWithHeaders(rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isPNG(b []byte) bool {
	return len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G'
}

func makePNG(w, h int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
