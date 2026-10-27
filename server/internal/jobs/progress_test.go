package jobs

import (
	"fmt"
	"sync"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// TestSingleChapterProgressAdvances 锁住这个真实问题：
//
//	wnacg（绅士漫画）这类画廊型源 loadInfo 返回 chapters=0，整本按
//	"一个章节"下载。早期进度只在**章节之间**推进（progress = i/total），
//	total=1 时整个下载过程恒为 0/1 = 0%，管理面板看起来"卡死在 0%"，
//	但图片其实一直在正常下载。
//
// 修复要点：章内按图片完成数把进度铺满该章节所占区间；
// 且 1.0 只在收尾时写入，避免未完成就显示 100%。
func TestSingleChapterProgressAdvances(t *testing.T) {
	st := newTestStore(t)

	pages := make([]string, 40)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://example.test/%d.jpg", i)
	}
	run := &fakeRunner{pages: pages}
	dl := NewDownloader(st, run, 2)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return pngBytes(), nil
	})

	jobID, err := st.CreateJob(&model.Job{Source: "s", ComicID: "c", Title: "t"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	// 单章漫画：Chapters 为空，RunJob 应合成一章（章节 ID = 漫画 ID）
	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c", Title: "t"}}

	var mu sync.Mutex
	seen := []float64{}
	store.OnJobProgress = func(id int64, progress float64, message string) {
		mu.Lock()
		seen = append(seen, progress)
		mu.Unlock()
	}
	defer func() { store.OnJobProgress = nil }()

	if err := dl.RunJob(jobID, detail); err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	job, _ := st.GetJob(jobID)
	if job == nil || job.Status != model.JobDone {
		t.Fatalf("expected done, got %+v", job)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("expected multiple in-chapter progress updates, got %d (%v)", len(seen), seen)
	}
	// 至少要有中间态：>0 且 <1
	hasMid := false
	for _, p := range seen {
		if p > 0 && p < 1 {
			hasMid = true
		}
		if p > 0.999 && p < 1 {
			t.Fatalf("progress must not linger just below 1.0, got %v", p)
		}
	}
	if !hasMid {
		t.Fatalf("single-chapter job never reported intermediate progress: %v", seen)
	}
	// 单调不减
	prev := -1.0
	for _, p := range seen {
		if p < prev {
			t.Fatalf("progress went backwards: %v", seen)
		}
		prev = p
	}
}

// TestInChapterProgressCallbackCountsAllImages 回调必须覆盖**每一张**图片，
// 否则进度会停在中间（用户看到"卡在 40/159 不动"）。
//
// 注意：本测试只关心回调次数与最终值，不关心章节成功与否。
// 现在"缺页即失败"是既定策略（见 TestAnyFailedImageFailsChapter），
// 所以这里让所有图都成功，专注验证回调本身。
func TestInChapterProgressCallbackCountsAllImages(t *testing.T) {
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

	var mu sync.Mutex
	var lastDone, lastTotal int
	calls := 0
	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c"}}
	err := dl.downloadChapterProgress(detail, model.Chapter{ID: "1"}, t.TempDir(),
		func(done, total int) {
			mu.Lock()
			lastDone, lastTotal = done, total
			calls++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("all pages succeeded, chapter should be cached: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("progress callback should fire once per image, got %d", calls)
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Fatalf("final callback = %d/%d, want 3/3", lastDone, lastTotal)
	}
}

// TestProgressCountsFailedImagesToo 失败页也要计入进度，
// 否则缺页章节的进度条会永远差最后一格。
// 章节本身仍然会失败（缺页即失败），但回调必须走完。
func TestProgressCountsFailedImagesToo(t *testing.T) {
	st := newTestStore(t)
	pages := []string{
		"https://example.test/a.jpg",
		"https://example.test/b.jpg",
		"https://example.test/c.jpg",
	}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 1)
	dl.SetFetch(func(url string, _ map[string]string) ([]byte, error) {
		if url == "https://example.test/b.jpg" {
			return nil, fmt.Errorf("status 404")
		}
		return pngBytes(), nil
	})

	var mu sync.Mutex
	var lastDone, lastTotal int
	calls := 0
	detail := &model.ComicDetail{Comic: model.Comic{Source: "s", ID: "c2"}}
	_ = dl.downloadChapterProgress(detail, model.Chapter{ID: "1"}, t.TempDir(),
		func(done, total int) {
			mu.Lock()
			lastDone, lastTotal = done, total
			calls++
			mu.Unlock()
		})

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("progress must count failed images too, got %d callbacks", calls)
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Fatalf("final callback = %d/%d, want 3/3", lastDone, lastTotal)
	}
}
