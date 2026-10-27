package jobs

import (
	"fmt"
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

// TestChapterToleratesFewImageFailures 锁住这个真实问题：
//
//	上游图片 CDN 零星失败（picacg 200 页里挂 3 张、jm 45 页里挂 6 张）
//	曾经会让整章失败 → 整本失败 → 每次重试都从头再来，永远下不完。
//
// 现在的语义：缺页数在阈值内就当成成功，缺口靠客户端回落到在线源。
func TestChapterToleratesFewImageFailures(t *testing.T) {
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

// TestMaxFailedImages 校验阈值本身。
func TestMaxFailedImages(t *testing.T) {
	cases := []struct{ total, want int }{
		{0, 0}, {1, 2}, {10, 2}, {20, 2}, {30, 3}, {200, 20},
	}
	for _, c := range cases {
		if got := maxFailedImages(c.total); got != c.want {
			t.Errorf("maxFailedImages(%d) = %d, want %d", c.total, got, c.want)
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
