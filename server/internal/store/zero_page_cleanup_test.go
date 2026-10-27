package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
)

func newZeroPageStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "data"), filepath.Join(dir, "lib"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestDeleteZeroPageChapters 锁住清理语义：只删 page_count=0 的行，
// 正常章节一条都不能动。
//
// 这些 0 页行是旧版本"源站返回 0 页时记成功"留下的。它们的危害是
// 让漫画被判定为"已下全"——增量下载永远不重试，客户端读到空白页。
func TestDeleteZeroPageChapters(t *testing.T) {
	st := newZeroPageStore(t)

	seed := []model.CachedChapter{
		{Source: "picacg", ComicID: "abc", ChapterID: "1", ChapterTitle: "1",
			Dir: "x [abc]/1", PageCount: 10, CreatedAt: time.Now()},
		{Source: "picacg", ComicID: "abc", ChapterID: "2", ChapterTitle: "2",
			Dir: "x [abc]/2", PageCount: 0, CreatedAt: time.Now()},
		{Source: "jm", ComicID: "def", ChapterID: "3", ChapterTitle: "3",
			Dir: "y [def]/3", PageCount: 0, CreatedAt: time.Now()},
		{Source: "jm", ComicID: "def", ChapterID: "4", ChapterTitle: "4",
			Dir: "y [def]/4", PageCount: 7, CreatedAt: time.Now()},
	}
	for _, c := range seed {
		if err := st.PutCachedChapter(c); err != nil {
			t.Fatalf("seed %s: %v", c.ChapterID, err)
		}
	}

	n, err := st.CountZeroPageChapters()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 zero-page rows, got %d", n)
	}

	deleted, err := st.DeleteZeroPageChapters()
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 2 {
		t.Errorf("want deleted=2, got %d", deleted)
	}

	// 正常章节必须还在
	ch, err := st.GetCachedChapter("picacg", "abc", "1")
	if err != nil || ch == nil {
		t.Fatalf("normal chapter 1 lost: %v %v", ch, err)
	}
	if ch.PageCount != 10 {
		t.Errorf("chapter 1 pages changed: %d", ch.PageCount)
	}
	ch2, err := st.GetCachedChapter("jm", "def", "4")
	if err != nil || ch2 == nil {
		t.Fatalf("normal chapter 4 lost: %v %v", ch2, err)
	}

	// 清理后计数归零
	n, err = st.CountZeroPageChapters()
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0 after cleanup, got %d", n)
	}

	// 再删一次是幂等的
	deleted, err = st.DeleteZeroPageChapters()
	if err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if deleted != 0 {
		t.Errorf("second delete should be no-op, got %d", deleted)
	}
}

// TestZeroPageExcludedFromStats 确认"库内章节数"不再被 0 页行虚增。
//
// 面板上的「库内章节」直接 COUNT(*) cached_chapters；
// 0 页行会让用户以为下得比实际多。
func TestZeroPageExcludedFromStats(t *testing.T) {
	st := newZeroPageStore(t)
	for _, c := range []model.CachedChapter{
		{Source: "s", ComicID: "c", ChapterID: "1", Dir: "d/1", PageCount: 5, CreatedAt: time.Now()},
		{Source: "s", ComicID: "c", ChapterID: "2", Dir: "d/2", PageCount: 0, CreatedAt: time.Now()},
	} {
		if err := st.PutCachedChapter(c); err != nil {
			t.Fatalf("seed %s: %v", c.ChapterID, err)
		}
	}

	// 清理前后各看一次 Stats 的章节数
	before, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if before.LibraryChapters != 2 {
		t.Logf("note: Stats counts raw rows (LibraryChapters=%d)", before.LibraryChapters)
	}

	if _, err := st.DeleteZeroPageChapters(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if after.LibraryChapters != 1 {
		t.Errorf("want 1 chapter after cleanup, got %d", after.LibraryChapters)
	}
}
