package store

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// 章节列表必须**按源端顺序**原样保存。
//
// 这是离线兜底详情页正确性的根：客户端阅读器把 history.ep 当作下标
// 去章节数组里取章节，顺序一旦被压缩或重排，打开的就是错的章节。
// 实测过的真实故障：历史显示第 19 章，兜底详情页却推出 E39。
func TestChapterListOrderIsPreserved(t *testing.T) {
	s := newTestStore(t)

	full := []model.Chapter{
		{ID: "c1", Title: "19"},
		{ID: "c2", Title: "18 前篇"},
		{ID: "c3", Title: "18 後篇"},
		{ID: "c4", Title: "17"},
	}
	if err := s.SetComicChapterList("jm", "143092", full); err != nil {
		t.Fatalf("SetComicChapterList: %v", err)
	}

	got, err := s.GetComicChapterList("jm", "143092")
	if err != nil {
		t.Fatalf("GetComicChapterList: %v", err)
	}
	if len(got) != len(full) {
		t.Fatalf("chapter count = %d, want %d", len(got), len(full))
	}
	for i := range full {
		if got[i].ID != full[i].ID || got[i].Title != full[i].Title {
			t.Fatalf("chapter[%d] = %s/%q, want %s/%q",
				i, got[i].ID, got[i].Title, full[i].ID, full[i].Title)
		}
	}
}

// 空列表不得覆盖已有记录：源偶尔会返回空章节表，
// 用它覆盖只会把原本正确的索引搞坏。
func TestEmptyChapterListDoesNotOverwrite(t *testing.T) {
	s := newTestStore(t)

	full := []model.Chapter{{ID: "c1", Title: "1"}}
	if err := s.SetComicChapterList("jm", "1", full); err != nil {
		t.Fatalf("SetComicChapterList: %v", err)
	}
	if err := s.SetComicChapterList("jm", "1", nil); err != nil {
		t.Fatalf("SetComicChapterList(nil): %v", err)
	}

	got, err := s.GetComicChapterList("jm", "1")
	if err != nil {
		t.Fatalf("GetComicChapterList: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("chapter list was clobbered by empty write: %+v", got)
	}
}

// 只下过章节、没同步过元信息时也要能写（comics 行可能不存在）。
func TestChapterListUpsertsWithoutComicRow(t *testing.T) {
	s := newTestStore(t)

	// 故意不先 UpsertComic
	if err := s.SetComicChapterList("s", "no-meta", []model.Chapter{
		{ID: "a", Title: "A"},
	}); err != nil {
		t.Fatalf("SetComicChapterList without comic row: %v", err)
	}

	got, err := s.GetComicChapterList("s", "no-meta")
	if err != nil {
		t.Fatalf("GetComicChapterList: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("got %+v, want one chapter a", got)
	}
}

// 读取不存在的记录应返回 nil 而不是报错：调用方据此回退到
// "只给已缓存章节"的老行为。
func TestGetChapterListMissingReturnsNil(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetComicChapterList("nope", "nope")
	if err != nil {
		t.Fatalf("GetComicChapterList: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}
