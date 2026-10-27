package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// fakeRunner 是可控的 SourceRunner。
type fakeRunner struct {
	pages    []string
	loadErr  error
	cfgErr   error
	loadCall int32
}

func (f *fakeRunner) LoadDetail(string, string) (*model.ComicDetail, error) {
	return nil, nil
}
func (f *fakeRunner) LoadEpisodes(string, string, string) ([]string, error) {
	atomic.AddInt32(&f.loadCall, 1)
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.pages, nil
}
func (f *fakeRunner) ImageConfigFor(string, string, string, string) (*ImageConfig, error) {
	return &ImageConfig{}, f.cfgErr
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"), filepath.Join(dir, "lib"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// pngBytes 返回一段最小合法 PNG 头（imageExt 按魔数判断扩展名即可）。
func pngBytes() []byte {
	return []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
}

// TestTransientImageFailuresRecoveredByRetry 锁住重试的价值：
//
//	上游图片 CDN 零星失败（picacg 200 页里挂 3 张、jm 45 页里挂 6 张）
//	是常态，但绝大多数是瞬时的（EOF / 空 body / 限流 HTML），
//	隔几百毫秒重试就能成功。
//
// 注意这里和「缺页即失败」不冲突：
//   - 本测试：每张图**最终**都下到了 → 整章成功、PageCount=20
//   - TestAnyFailedImageFailsChapter：重试 3 次仍失败 → 整章失败、不落库
func TestTransientImageFailuresRecoveredByRetry(t *testing.T) {
	st := newTestStore(t)

	pages := make([]string, 20)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://example.test/%d.jpg", i)
	}
	run := &fakeRunner{pages: pages}

	dl := NewDownloader(st, run, 4)

	var calls int32
	dl.SetFetch(func(url string, _ map[string]string) ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		// 前 10 次调用里失败 2 次（分别在各页的首次尝试），重试会成功
		if n <= 2 {
			return nil, fmt.Errorf("Get %q: EOF", url)
		}
		return pngBytes(), nil
	})

	base := t.TempDir()
	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c"}}
	ch := model.Chapter{ID: "1", Title: "c1"}

	if err := dl.downloadChapter(detail, ch, base); err != nil {
		t.Fatalf("chapter must tolerate transient image failures, got %v", err)
	}

	got, err := st.GetCachedChapter("s", "c", "1")
	if err != nil || got == nil {
		t.Fatalf("chapter must be recorded as cached, err=%v", err)
	}
	if got.PageCount != 20 {
		t.Fatalf("all 20 pages eventually succeeded, expected PageCount=20, got %d", got.PageCount)
	}
}

// TestChapterFailsWhenMostImagesFail 确认阈值仍能拦住真正的失败。
func TestChapterFailsWhenMostImagesFail(t *testing.T) {
	st := newTestStore(t)

	pages := make([]string, 10)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://example.test/%d.jpg", i)
	}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 4)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return nil, fmt.Errorf("Get: connection reset by peer")
	})

	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c"}}
	err := dl.downloadChapter(detail, model.Chapter{ID: "1"}, t.TempDir())
	if err == nil {
		t.Fatal("chapter with all images failing must fail")
	}
}

// TestImageRetryIsBounded 确认重试次数有限，且不发散到不可控。
func TestImageRetryIsBounded(t *testing.T) {
	st := newTestStore(t)
	pages := []string{"https://example.test/0.jpg"}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 1)

	var calls int32
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("Get: EOF")
	})

	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c"}}
	_ = dl.downloadChapter(detail, model.Chapter{ID: "1"}, t.TempDir())

	// 单页 + 3 次尝试 = 3 次 fetch
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected exactly 3 attempts (1 + 2 retries), got %d", got)
	}
}

// TestNonRetriableImageErrorSkipsRetry 确认 404/403 不浪费重试。
func TestNonRetriableImageErrorSkipsRetry(t *testing.T) {
	st := newTestStore(t)
	pages := []string{"https://example.test/0.jpg"}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 1)

	var calls int32
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("image fetch: status 404 for https://example.test/0.jpg")
	})

	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c"}}
	_ = dl.downloadChapter(detail, model.Chapter{ID: "1"}, t.TempDir())

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("404 must not be retried, got %d attempts", got)
	}
}

// TestAnyFailedImageFailsChapter 锁住"缺页即失败"的策略。
//
// 历史行为是容忍 total/10（至少 2 页）以内的缺页，理由是"重下整章
// 代价太高"。实际后果更糟：
//   - 缺的那几页在客户端是 404 / 空白
//   - 章节被标记为已缓存 → 增量下载永远不再尝试
//   - 库里看起来完整，问题被永久掩盖
//
// 现在的语义：任何一张图重试后仍失败 → 整章失败、不落库。
func TestAnyFailedImageFailsChapter(t *testing.T) {
	st := newTestStore(t)

	// 20 页里只让第 7 页失败（远低于旧阈值 2 页/10% 的容忍线）
	pages := make([]string, 20)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://example.test/%d.jpg", i)
	}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 2)
	dl.SetFetch(func(url string, _ map[string]string) ([]byte, error) {
		if url == "https://example.test/7.jpg" {
			return nil, fmt.Errorf("status 404")
		}
		return pngBytes(), nil
	})

	detail := &model.ComicDetail{
		Comic:    model.Comic{Source: "s", ID: "c", Title: "t"},
		Chapters: []model.Chapter{{ID: "1", Title: "ch1"}},
	}
	libDir := st.LibraryDir()
	err := dl.downloadChapter(detail, detail.Chapters[0], libDir)
	if err == nil {
		t.Fatal("single failed image must fail the whole chapter, got nil")
	}
	t.Logf("error (expected): %v", err)

	// 不能入库
	ids, err := st.CachedChapterIDs("s", "c")
	if err != nil {
		t.Fatalf("CachedChapterIDs: %v", err)
	}
	if ids["1"] {
		t.Error("chapter with missing images must NOT be recorded as cached")
	}

	// 半成品目录要清掉，避免和下次重试的编号混在一起
	entries, _ := os.ReadDir(filepath.Join(libDir, chapterDirName(detail.Chapters[0])))
	if len(entries) > 0 {
		t.Errorf("partial chapter dir should be cleaned, found %d files", len(entries))
	}
}

// TestPagesWrittenContiguously 锁住"文件编号必须连续"。
//
// 客户端用 `0..PageCount-1` 拼图片 URL（见 api.handleChapter），
// 一旦落盘编号有空洞就会 404。旧实现按原始索引命名，遇到失败会留下
// 空洞 + 结尾缺页，正是用户报告的"部分页没下载完成"。
func TestPagesWrittenContiguously(t *testing.T) {
	st := newTestStore(t)
	pages := []string{
		"https://example.test/a.jpg",
		"https://example.test/b.jpg",
		"https://example.test/c.jpg",
	}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 1)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return pngBytes(), nil
	})

	detail := &model.ComicDetail{
		Comic:    model.Comic{Source: "s", ID: "c2", Title: "t"},
		Chapters: []model.Chapter{{ID: "1", Title: "ch1"}},
	}
	if err := dl.downloadChapter(detail, detail.Chapters[0], st.LibraryDir()); err != nil {
		t.Fatalf("downloadChapter: %v", err)
	}

	ch, err := st.GetCachedChapter("s", "c2", "1")
	if err != nil || ch == nil {
		t.Fatalf("chapter should be cached: %v %v", ch, err)
	}
	if ch.PageCount != 3 {
		t.Fatalf("want 3 pages, got %d", ch.PageCount)
	}

	// 磁盘上必须是 0,1,2 —— 少一个客户端就会 404
	dir := filepath.Join(st.LibraryDir(), filepath.FromSlash(ch.Dir))
	for i := 0; i < ch.PageCount; i++ {
		matches, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%d.*", i)))
		if len(matches) == 0 {
			t.Errorf("page %d missing on disk (client would 404)", i)
		}
	}
}

// chapterAwareRunner 让失败只作用于指定章节。
//
// fakeRunner.loadErr 对所有章节一视同仁，无法表达"第 1 章成功、
// 第 2 章失败"这种真实场景（源站单章 500）。
type chapterAwareRunner struct {
	ok   *fakeRunner
	fail error
}

func (c *chapterAwareRunner) LoadDetail(k, id string) (*model.ComicDetail, error) {
	return c.ok.LoadDetail(k, id)
}

func (c *chapterAwareRunner) LoadEpisodes(k, comicID, ep string) ([]string, error) {
	if ep == "2" {
		return nil, c.fail
	}
	return c.ok.LoadEpisodes(k, comicID, ep)
}

func (c *chapterAwareRunner) ImageConfigFor(k, comicID, ep, img string) (*ImageConfig, error) {
	return c.ok.ImageConfigFor(k, comicID, ep, img)
}

// TestJobContinuesAfterOneChapterFails 锁住"一章毁全本"的修复：
//
//	RunJob 现在会跳过失败章节继续往下，只有**全部**失败才判整本失败。
func TestJobContinuesAfterOneChapterFails(t *testing.T) {
	st := newTestStore(t)
	run := &fakeRunner{pages: []string{"https://example.test/0.jpg"}}
	dl := NewDownloader(st, run, 2)

	dl.SetFetch(func(url string, _ map[string]string) ([]byte, error) {
		if url == "https://example.test/0.jpg" {
			return pngBytes(), nil
		}
		return nil, fmt.Errorf("nope")
	})

	jobID, err := st.CreateJob(&model.Job{
		Source: "s", ComicID: "c", Title: "t", Origin: "test",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	detail := &model.ComicDetail{
		Comic: model.Comic{Source: "s", ID: "c", Title: "t"},
		Chapters: []model.Chapter{
			{ID: "1", Title: "ok"},
			{ID: "2", Title: "will fail"},
		},
	}

	// 让第 2 章加载页面失败（源站报错），第 1 章正常。
	//
	// 注意不能再用 `run.pages = nil` 来表达这件事：现在"0 页"是
	// 一个独立的、明确的失败语义（源站返回空章节），而"加载报错"
	// 应该用 loadErr。这里按章节 ID 区分，因为 fakeRunner 的
	// loadErr 是对所有章节生效的。
	base := run
	dl = NewDownloader(st, &chapterAwareRunner{
		ok:   base,
		fail: fmt.Errorf("load pages: Invalid status code: 500"),
	}, 2)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return pngBytes(), nil
	})
	if err := dl.RunJob(jobID, detail); err != nil {
		// 两个章节中一个成功一个失败 → 应判成功（部分完成）
		t.Fatalf("job should tolerate one failed chapter, got %v", err)
	}

	job, _ := st.GetJob(jobID)
	if job == nil {
		t.Fatal("job missing")
	}
	if job.Status != model.JobDone {
		t.Fatalf("expected done with partial chapters, got %s (%s)", job.Status, job.Message)
	}
}
