package jobs

import (
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
)

// 空的 chapters 列表（源站返回 0 页）必须**算失败**，且不得写进
// cached_chapters。
//
// 真实事故：picacg 6048f155 的第 71/72 话源站返回 0 页，
// 旧实现写了一条 PageCount=0 的记录当"成功"，于是
//   1. CachedChapterIDs 认为已缓存 → 增量下载永不重试，任务显示"已完成"；
//   2. handleChapter 返回 cached:true + pages:[] → 客户端打开是空白页，
//      而且因为 NAS 命中了，连在线兜底都不会走。
// 用户看到的是"两章空白，但下载管理器说这本下全了"。
func TestEmptyChapterIsFailureAndNotRecorded(t *testing.T) {
	st := newTestStore(t)
	// pages 为空 = 源站返回 0 页
	dl := NewDownloader(st, &fakeRunner{pages: nil}, 2)

	detail := &model.ComicDetail{
		Comic: model.Comic{Source: "picacg", ID: "6048f155", Title: "T"},
		Chapters: []model.Chapter{{ID: "71", Title: "第71話"}},
	}

	err := dl.downloadChapter(detail, detail.Chapters[0], st.LibraryDir())
	if err == nil {
		t.Fatal("want error for 0-page chapter, got nil")
	}
	t.Logf("error (expected): %v", err)

	// 关键断言：不能落库
	ids, err := st.CachedChapterIDs("picacg", "6048f155")
	if err != nil {
		t.Fatalf("CachedChapterIDs: %v", err)
	}
	if ids["71"] {
		t.Error("0-page chapter must NOT be recorded as cached")
	}

	ch, err := st.GetCachedChapter("picacg", "6048f155", "71")
	if err != nil {
		t.Fatalf("GetCachedChapter: %v", err)
	}
	if ch != nil {
		t.Errorf("0-page chapter must not be readable, got %+v", ch)
	}
}

// 存量的 0 页脏记录不能被当成"已缓存"。
//
// 已装机的库里已经有这种行（修复前写进去的），所以读取侧必须
// 自带过滤，否则用户永远等不到它们被重下。
func TestZeroPageRowsAreIgnoredOnRead(t *testing.T) {
	st := newTestStore(t)

	// 直接写一条 0 页记录，模拟修复前的脏数据
	if err := st.PutCachedChapter(model.CachedChapter{
		Source: "picacg", ComicID: "6048f155", ChapterID: "72",
		ChapterTitle: "第72話", Dir: "x [6048f155]/72",
		PageCount: 0, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed dirty row: %v", err)
	}

	ids, err := st.CachedChapterIDs("picacg", "6048f155")
	if err != nil {
		t.Fatalf("CachedChapterIDs: %v", err)
	}
	if ids["72"] {
		t.Error("0-page row must not count as cached (otherwise it never gets retried)")
	}

	ch, err := st.GetCachedChapter("picacg", "6048f155", "72")
	if err != nil {
		t.Fatalf("GetCachedChapter: %v", err)
	}
	if ch != nil {
		t.Error("0-page row must not be served to clients (would render a blank chapter)")
	}

	// 整本都是 0 页时，不应阻止重新入队
	has, err := st.HasCachedChapters("picacg", "6048f155")
	if err != nil {
		t.Fatalf("HasCachedChapters: %v", err)
	}
	if has {
		t.Error("book with only 0-page rows must be allowed back into the queue")
	}
}

// 正常章节不受影响：仍然入库、仍然算已缓存。
func TestNormalChapterStillRecorded(t *testing.T) {
	st := newTestStore(t)
	pages := []string{"https://example.test/1.jpg", "https://example.test/2.jpg"}
	dl := NewDownloader(st, &fakeRunner{pages: pages}, 2)
	dl.SetFetch(func(string, map[string]string) ([]byte, error) {
		return pngBytes(), nil
	})

	detail := &model.ComicDetail{
		Comic: model.Comic{Source: "jm", ID: "42", Title: "T"},
		Chapters: []model.Chapter{{ID: "1", Title: "第1話"}},
	}
	if err := dl.downloadChapter(detail, detail.Chapters[0], st.LibraryDir()); err != nil {
		t.Fatalf("downloadChapter: %v", err)
	}

	ch, err := st.GetCachedChapter("jm", "42", "1")
	if err != nil || ch == nil {
		t.Fatalf("chapter should be cached: %v %v", ch, err)
	}
	if ch.PageCount != 2 {
		t.Errorf("want 2 pages, got %d", ch.PageCount)
	}
}

// 混合情况：一本里既有正常章、也有 0 页章。
// 增量下载必须只把 0 页那章算进"缺失"，而不是整本判为已完成。
func TestIncrementalSeesZeroPageChapterAsMissing(t *testing.T) {
	st := newTestStore(t)

	// 正常章
	if err := st.PutCachedChapter(model.CachedChapter{
		Source: "picacg", ComicID: "abc", ChapterID: "1",
		ChapterTitle: "1", Dir: "x [abc]/1", PageCount: 10, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	// 脏章
	if err := st.PutCachedChapter(model.CachedChapter{
		Source: "picacg", ComicID: "abc", ChapterID: "2",
		ChapterTitle: "2", Dir: "x [abc]/2", PageCount: 0, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed dirty: %v", err)
	}

	ids, err := st.CachedChapterIDs("picacg", "abc")
	if err != nil {
		t.Fatalf("CachedChapterIDs: %v", err)
	}
	if !ids["1"] {
		t.Error("chapter 1 should be cached")
	}
	if ids["2"] {
		t.Error("chapter 2 (0 pages) must be reported as missing so increment re-downloads it")
	}

	// 用 source 端的完整章节表求差集，模拟 EnqueueIncremental 的判断
	srcChapters := []string{"1", "2"}
	var missing []string
	for _, id := range srcChapters {
		if !ids[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) != 1 || missing[0] != "2" {
		t.Errorf("want exactly chapter 2 missing, got %v", missing)
	}
}

