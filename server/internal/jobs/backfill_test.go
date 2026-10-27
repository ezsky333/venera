package jobs

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// 补录必须跳过已缓存的收藏。
//
// 这个入口的意义是"把收藏里还没下过的补进队列"。如果连已缓存的也入队，
// 每本都会白跑 loadInfo/loadEp 再全部 skip——既浪费上游请求，
// 又让队列看起来塞满了实际什么都不做的任务。
func TestQueuePlanSkipsCached(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustAddFav(t, st, "jm", "cached", "已缓存的书")
	mustAddFav(t, st, "jm", "fresh", "没下过的书")
	mustAddCached(t, st, "jm", "cached")

	plan, err := w.QueueFavoritesDetailed(true, "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Candidates != 1 {
		t.Fatalf("candidates = %d, want 1", plan.Candidates)
	}
	if plan.AlreadyCached != 1 {
		t.Fatalf("alreadyCached = %d, want 1", plan.AlreadyCached)
	}
	if len(plan.Items) != 1 || plan.Items[0].ComicID != "fresh" {
		t.Fatalf("items = %+v, want only 'fresh'", plan.Items)
	}
}

// 同一本漫画出现在多个收藏夹时只能算一次。
// 实测收藏表 991 行其实只有 447 本；不认重会让"要下多少本"看起来翻倍。
func TestQueuePlanDedupesAcrossFolders(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustAddFav(t, st, "jm", "X", "书X") // 三个文件夹都收了同一本
	mustAddFav(t, st, "jm", "X", "书X")
	mustAddFav(t, st, "jm", "X", "书X")

	plan, err := w.QueueFavoritesDetailed(true, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Candidates != 1 {
		t.Fatalf("candidates = %d, want 1 (deduped)", plan.Candidates)
	}
}

// dryRun 绝不能写库。
func TestQueuePlanDryRunWritesNothing(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustAddFav(t, st, "jm", "A", "书A")
	mustAddFav(t, st, "picacg", "B", "书B")

	plan, err := w.QueueFavoritesDetailed(true, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Candidates != 2 {
		t.Fatalf("candidates = %d, want 2", plan.Candidates)
	}
	if plan.Queued != 0 {
		t.Fatalf("queued = %d, want 0 on dryRun", plan.Queued)
	}
	if !plan.DryRun {
		t.Error("DryRun flag not set")
	}
	n := jobCount(t, st)
	if n != 0 {
		t.Fatalf("dryRun created %d jobs, want 0", n)
	}
}

// 执行时按源正确分类，且真的入队。
func TestQueueFavoritesDetailedQueuesAndGroups(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustAddFav(t, st, "jm", "J1", "jm书1")
	mustAddFav(t, st, "jm", "J2", "jm书2")
	mustAddFav(t, st, "picacg", "P1", "picacg书")

	plan, err := w.QueueFavoritesDetailed(false, "favorite-backfill")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Queued != 3 {
		t.Fatalf("queued = %d, want 3", plan.Queued)
	}
	if plan.BySource["jm"] != 2 || plan.BySource["picacg"] != 1 {
		t.Fatalf("bySource = %+v, want jm:2 picacg:1", plan.BySource)
	}
	n := jobCount(t, st)
	if n != 3 {
		t.Fatalf("jobs in db = %d, want 3", n)
	}

	// 再跑一次：全部已在队列，不该重复入队
	plan2, err := w.QueueFavoritesDetailed(false, "favorite-backfill")
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Queued != 0 {
		t.Fatalf("second run queued = %d, want 0 (all pending)", plan2.Queued)
	}
	if plan2.AlreadyQueued != 3 {
		t.Fatalf("alreadyQueued = %d, want 3", plan2.AlreadyQueued)
	}
}

// 明细最多返回 previewLimit 条，避免一次返回几百条撑爆面板。
func TestQueuePlanItemsCapped(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	for i := 0; i < previewLimit+20; i++ {
		mustAddFav(t, st, "jm", "C"+itoa(i), "书")
	}
	plan, err := w.QueueFavoritesDetailed(true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != previewLimit {
		t.Fatalf("items = %d, want capped at %d", len(plan.Items), previewLimit)
	}
	if plan.Candidates != previewLimit+20 {
		t.Fatalf("candidates = %d, want %d", plan.Candidates, previewLimit+20)
	}
}

// ---------- helpers ----------

func itoa(i int) string {
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

func mustAddFav(t *testing.T, st *store.Store, source, comicID, title string) {
	t.Helper()
	if _, err := st.AddFavorite(model.Favorite{
		Comic:  model.Comic{Source: source, ID: comicID, Title: title},
		Folder: "测试夹",
	}, 1); err != nil {
		t.Fatalf("add favorite %s/%s: %v", source, comicID, err)
	}
}

func mustAddCached(t *testing.T, st *store.Store, source, comicID string) {
	t.Helper()
	if err := st.PutCachedChapter(model.CachedChapter{
		Source: source, ComicID: comicID, ChapterID: "ch1",
		ChapterTitle: "第1话", Dir: "d", PageCount: 1,
	}); err != nil {
		t.Fatalf("put cached chapter: %v", err)
	}
}

// jobCount 数一下 jobs 表当前有多少行。
func jobCount(t *testing.T, st *store.Store) int {
	t.Helper()
	_, total, err := st.ListJobs("", 1, 0)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return total
}

// newWorkerForTest 造一个不启动 goroutine 的 worker（只用来调规划逻辑）。
func newWorkerForTest(st *store.Store) *Worker {
	return NewWorker(st, &fakeRunner{}, 1)
}
