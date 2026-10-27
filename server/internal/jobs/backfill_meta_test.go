package jobs

import (
	"errors"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
	_ "modernc.org/sqlite"
)

// fakeLoader 记录被询问过的条目，并按 comicID 返回预置结果。
type fakeLoader struct {
	calls  []string
	detail map[string]*model.ComicDetail
	errs   map[string]error
}

func (f *fakeLoader) LoadDetail(key, comicID string) (*model.ComicDetail, error) {
	f.calls = append(f.calls, key+"/"+comicID)
	if err := f.errs[comicID]; err != nil {
		return nil, err
	}
	if d, ok := f.detail[comicID]; ok {
		return d, nil
	}
	return nil, nil
}

func newMetaStore(t *testing.T) *store.Store {
	t.Helper()
	// store.Open 需要目录；用临时目录即可。
	dir := t.TempDir()
	st, err := store.Open(dir, dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestBackfillMetaFillsMissingFields 锁住核心语义：
// 只缺元数据的条目会被回源补齐，作者/简介写进 comics 表。
func TestBackfillMetaFillsMissingFields(t *testing.T) {
	st := newMetaStore(t)

	// 只有 title/cover 的条目（模拟"只同步过收藏"）。
	if err := st.UpsertComic(model.Comic{
		Source: "ehentai", ID: "https://e-hentai.org/g/1/abc/",
		Title: "Comic One", Cover: "https://img/1.jpg",
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	loader := &fakeLoader{detail: map[string]*model.ComicDetail{
		"https://e-hentai.org/g/1/abc/": {
			Comic: model.Comic{
				Source: "ehentai", ID: "https://e-hentai.org/g/1/abc/",
				Title: "Comic One", Author: "Artist A", Uploader: "Uploader B",
				Description: "A description", Tags: []string{"tag1", "tag2"},
			},
		},
	}}

	bf := NewMetaBackfiller(st, loader, nil)
	res, err := bf.Run("", 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total != 1 || res.Updated != 1 {
		t.Fatalf("want total=1 updated=1, got %+v", res)
	}
	if len(loader.calls) != 1 {
		t.Fatalf("want 1 loader call, got %v", loader.calls)
	}

	c, err := st.GetComic("ehentai", "https://e-hentai.org/g/1/abc/")
	if err != nil || c == nil {
		t.Fatalf("GetComic: %v %v", c, err)
	}
	if c.Author != "Artist A" {
		t.Errorf("author not backfilled: %q", c.Author)
	}
	if c.Description != "A description" {
		t.Errorf("description not backfilled: %q", c.Description)
	}

	// 补全后不应再有"待补全"条目。
	n, err := st.CountComicsMissingMeta()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0 missing after backfill, got %d", n)
	}
}

// TestBackfillMetaKeepsExistingAuthor 确认已有作者不被源站覆盖。
//
// 用户可能手工改过作者，或某个源返回的作者是错的；
// 补全的定位是"填空"，不是"覆盖"。
func TestBackfillMetaKeepsExistingAuthor(t *testing.T) {
	st := newMetaStore(t)

	// author 已有值，description 缺失 → 仍属于"待补全"。
	if err := st.UpsertComic(model.Comic{
		Source: "jm", ID: "42", Title: "T", Author: "Manual Author",
	}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	loader := &fakeLoader{detail: map[string]*model.ComicDetail{
		"42": {Comic: model.Comic{
			Source: "jm", ID: "42", Title: "T",
			Author: "From Source", Description: "Desc",
		}},
	}}
	bf := NewMetaBackfiller(st, loader, nil)
	if _, err := bf.Run("", 0); err != nil {
		t.Fatalf("Run: %v", err)
	}

	c, err := st.GetComic("jm", "42")
	if err != nil || c == nil {
		t.Fatalf("GetComic: %v %v", c, err)
	}
	if c.Author != "Manual Author" {
		t.Errorf("existing author was overwritten: %q", c.Author)
	}
	if c.Description != "Desc" {
		t.Errorf("missing description not filled: %q", c.Description)
	}
}

// TestBackfillMetaCountsFailures 确认源站报错时如实记失败，不假装成功。
func TestBackfillMetaCountsFailures(t *testing.T) {
	st := newMetaStore(t)
	if err := st.UpsertComic(model.Comic{Source: "jm", ID: "7", Title: "T"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	loader := &fakeLoader{errs: map[string]error{
		"7": errors.New("Invalid status code: 404"),
	}}
	bf := NewMetaBackfiller(st, loader, nil)
	res, err := bf.Run("", 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Failed != 1 || res.Updated != 0 {
		t.Fatalf("want failed=1 updated=0, got %+v", res)
	}
	if len(res.Errors) == 0 {
		t.Error("failure reason not reported")
	}
	// 失败条目仍应算"待补全"，这样用户可以再点一次。
	if res.Remaining != 1 {
		t.Errorf("want remaining=1, got %d", res.Remaining)
	}
}

// TestBackfillMetaSkipsWhenSourceHasNothing 确认源站也拿不到信息时记为跳过，
// 不写空值、也不算失败。
func TestBackfillMetaSkipsWhenSourceHasNothing(t *testing.T) {
	st := newMetaStore(t)
	if err := st.UpsertComic(model.Comic{Source: "jm", ID: "8", Title: "T"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	loader := &fakeLoader{detail: map[string]*model.ComicDetail{
		"8": {Comic: model.Comic{Source: "jm", ID: "8", Title: "T"}},
	}}
	bf := NewMetaBackfiller(st, loader, nil)
	res, err := bf.Run("", 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != 1 || res.Updated != 0 || res.Failed != 0 {
		t.Fatalf("want skipped=1, got %+v", res)
	}
}

// TestBackfillMetaFilterBySource 确认 source 过滤生效（面板/App 都按源补）。
func TestBackfillMetaFilterBySource(t *testing.T) {
	st := newMetaStore(t)
	for _, c := range []model.Comic{
		{Source: "jm", ID: "1", Title: "A"},
		{Source: "ehentai", ID: "2", Title: "B"},
	} {
		if err := st.UpsertComic(c, 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	loader := &fakeLoader{detail: map[string]*model.ComicDetail{
		"1": {Comic: model.Comic{Source: "jm", ID: "1", Author: "X"}},
	}}
	bf := NewMetaBackfiller(st, loader, nil)
	res, err := bf.Run("jm", 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("want total=1 for source filter, got %+v", res)
	}
	if len(loader.calls) != 1 || loader.calls[0] != "jm/1" {
		t.Fatalf("unexpected loader calls: %v", loader.calls)
	}
	// ehentai 那条仍待补全（Remaining 是全库口径）。
	//
	// jm/1 这次源站只给了 Author，description 仍空 —— 所以它也算"待补全"。
	// 这里断言的是"过滤只处理了 jm"，即 ehentai 根本没被触碰。
	loaderCalls := len(loader.calls)
	if loaderCalls != 1 {
		t.Fatalf("source filter leaked: %v", loader.calls)
	}
	c, _ := st.GetComic("ehentai", "2")
	if c != nil && (c.Author != "" || c.Description != "") {
		t.Errorf("ehentai row should be untouched, got author=%q desc=%q",
			c.Author, c.Description)
	}
}

// 确认 store 的 ListHistories 不再截断（快照必须带全量历史）。
//
// 这是"新设备同步完历史少一截"的回归防线：曾经 >500 会被折回 50，
// 而 syncer 又硬编码传 500，两条叠加导致全量快照只带 500 条。
func TestListHistoriesNoTruncation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	const want = 612
	for i := 0; i < want; i++ {
		h := model.History{
			Comic: model.Comic{
				Source: "jm",
				ID:     "c" + metaID(i),
				Title:  "t",
			},
			UpdatedAt: time.UnixMilli(int64(1700000000000 + i*1000)),
		}
		if _, err := st.UpsertHistory(h); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	all, err := st.ListHistories(0)
	if err != nil {
		t.Fatalf("ListHistories(0): %v", err)
	}
	if len(all) != want {
		t.Fatalf("want %d histories, got %d", want, len(all))
	}

	limited, err := st.ListHistories(10)
	if err != nil {
		t.Fatalf("ListHistories(10): %v", err)
	}
	if len(limited) != 10 {
		t.Fatalf("want 10 with explicit limit, got %d", len(limited))
	}
}

func metaID(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

