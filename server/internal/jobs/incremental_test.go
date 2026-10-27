package jobs

import (
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
)

// mustSetChapterList 写入源端完整章节表。
func mustSetChapterList(t *testing.T, st interface {
	SetComicChapterList(source, comicID string, list []model.Chapter) error
}, source, comicID string, ids ...string) {
	t.Helper()
	list := make([]model.Chapter, 0, len(ids))
	for _, id := range ids {
		list = append(list, model.Chapter{ID: id, Title: "第" + id + "话"})
	}
	if err := st.SetComicChapterList(source, comicID, list); err != nil {
		t.Fatalf("set chapter list: %v", err)
	}
}

// mustCache 写入一章缓存记录。
func mustCache(t *testing.T, st interface {
	PutCachedChapter(model.CachedChapter) error
}, source, comicID, chapterID string) {
	t.Helper()
	if err := st.PutCachedChapter(model.CachedChapter{
		Source: source, ComicID: comicID, ChapterID: chapterID,
		ChapterTitle: "第" + chapterID + "话", Dir: "d/" + chapterID, PageCount: 1,
	}); err != nil {
		t.Fatalf("put cached chapter: %v", err)
	}
}

// 核心用例：已缓存 2 章、源端有 4 章 → 只补后 2 章。
//
// 这正是用户报的问题："已缓存的漫画更新了新章节，点推送到 NAS
// 会不会触发新章节下载"。旧实现在这里返回 409 already cached，
// 后 2 章永远下不到。
func TestEnqueueIncrementalOnlyMissingChapters(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustCache(t, st, "jm", "C1", "1")
	mustCache(t, st, "jm", "C1", "2")
	mustSetChapterList(t, st, "jm", "C1", "1", "2", "3", "4")

	res, err := w.EnqueueIncremental("jm", "C1", "书", "manual", nil)
	if err != nil {
		t.Fatalf("enqueue incremental: %v", err)
	}
	if !res.Queued {
		t.Fatal("expected a job to be queued for the 2 new chapters")
	}
	if res.Total != 4 || res.Cached != 2 {
		t.Fatalf("total/cached = %d/%d, want 4/2", res.Total, res.Cached)
	}
	if len(res.Missing) != 2 {
		t.Fatalf("missing = %v, want 2 chapters", res.Missing)
	}
	if res.Missing[0] != "3" || res.Missing[1] != "4" {
		t.Fatalf("missing = %v, want [3 4]", res.Missing)
	}

	// job.chapters 必须只含缺失章节，否则 worker 会整本重跑
	job, err := st.GetJob(res.JobID)
	if err != nil || job == nil {
		t.Fatalf("get job: %v / %v", err, job)
	}
	if len(job.Chapters) != 2 || job.Chapters[0] != "3" || job.Chapters[1] != "4" {
		t.Fatalf("job chapters = %v, want [3 4]", job.Chapters)
	}
}

// 全部章节都已缓存 → 不入队，返回 up-to-date（不是错误）。
func TestEnqueueIncrementalUpToDate(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	for _, id := range []string{"1", "2"} {
		mustCache(t, st, "jm", "C2", id)
	}
	mustSetChapterList(t, st, "jm", "C2", "1", "2")

	res, err := w.EnqueueIncremental("jm", "C2", "书", "manual", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Queued {
		t.Fatal("should not queue when everything is cached")
	}
	if res.Reason != "up-to-date" {
		t.Fatalf("reason = %q, want up-to-date", res.Reason)
	}
	if n := jobCount(t, st); n != 0 {
		t.Fatalf("jobs = %d, want 0 (no empty job row)", n)
	}
}

// 完全没有缓存 → 全量入队，job.chapters 为空表示"全部"。
func TestEnqueueIncrementalFromScratch(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustSetChapterList(t, st, "jm", "C3", "1", "2", "3")

	res, err := w.EnqueueIncremental("jm", "C3", "书", "manual", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Queued || res.Total != 3 || res.Cached != 0 {
		t.Fatalf("res = %+v, want queued with total=3 cached=0", res)
	}
	job, _ := st.GetJob(res.JobID)
	if len(job.Chapters) != 3 {
		t.Fatalf("job chapters = %v, want all 3", job.Chapters)
	}
}

// 章节表未知（早期下载没记录 chapter_list）：有缓存就视为已完整，
// 不把 126 本存量漫画全部重跑一遍。
func TestEnqueueIncrementalLegacyNoChapterList(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustCache(t, st, "jm", "C4", "whatever")

	res, err := w.EnqueueIncremental("jm", "C4", "书", "manual", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Queued {
		t.Fatal("legacy comic with cache and no chapter list should not be re-queued")
	}
	if res.Reason != "up-to-date" {
		t.Fatalf("reason = %q", res.Reason)
	}
}

// 画廊型源：章节表为空时，整本一条记录（chapter_id == comicID）即视为完整。
//
// 注意与下一个用例的区别：**章节表存在**时绝不能再用 chapter_id==comicID
// 当"整本"标志，因为 jm 的章节 id 可能正好等于漫画 id。
func TestEnqueueIncrementalWholeBookRecord(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	// 章节表为空 = 画廊型源
	mustCache(t, st, "wnacg", "9001", "9001")

	res, err := w.EnqueueIncremental("wnacg", "9001", "书", "manual", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Queued {
		t.Fatal("gallery source with a cached whole-book record must be up-to-date")
	}
	if res.Reason != "up-to-date" {
		t.Fatalf("reason = %q", res.Reason)
	}
}

// 回归：章节 id 恰好等于漫画 id 时不能把整本判成已缓存。
//
// 实测 jm/330218 的章节表里有 id="330218" 的一章，旧实现用
// cachedSet[comicID] 判断"整本记录"，于是 11 章全被当成已缓存，
// 真正缺的 3 章永远补不上（表现为点了推送却什么都不发生）。
func TestEnqueueIncrementalChapterIDEqualsComicID(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	// 章节表里有一章的 id 与漫画 id 相同
	mustSetChapterList(t, st, "jm", "330218", "330218", "482766", "519156", "623181")
	// 只缓存了"同 id 那一章"，另外 3 章没下
	mustCache(t, st, "jm", "330218", "330218")

	res, err := w.EnqueueIncremental("jm", "330218", "书", "manual", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Queued {
		t.Fatal("chapter id == comic id must NOT be treated as a whole-book record")
	}
	if len(res.Missing) != 3 {
		t.Fatalf("missing = %v, want the 3 uncached chapters", res.Missing)
	}
	for _, id := range res.Missing {
		if id == "330218" {
			t.Fatalf("missing must not contain the already-cached chapter: %v", res.Missing)
		}
	}
}

// 已有 pending/running 任务时不重复入队。
func TestEnqueueIncrementalAlreadyQueued(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustSetChapterList(t, st, "jm", "C5", "1")
	if _, err := w.EnqueueIncremental("jm", "C5", "书", "manual", nil); err != nil {
		t.Fatal(err)
	}
	_, err := w.EnqueueIncremental("jm", "C5", "书", "manual", nil)
	if err != ErrAlreadyQueued {
		t.Fatalf("err = %v, want ErrAlreadyQueued", err)
	}
}

// 显式指定章节时不做差集（用户点名要重下某章也该允许）。
func TestEnqueueIncrementalExplicitChaptersBypassDiff(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustCache(t, st, "jm", "C6", "1")
	mustSetChapterList(t, st, "jm", "C6", "1", "2")

	res, err := w.EnqueueIncremental("jm", "C6", "书", "manual", []string{"1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Queued {
		t.Fatal("explicit chapters must be honoured even if already cached")
	}
	job, _ := st.GetJob(res.JobID)
	if len(job.Chapters) != 1 || job.Chapters[0] != "1" {
		t.Fatalf("job chapters = %v, want [1]", job.Chapters)
	}
}

// 补录：部分缓存的书要算进 Incremental 并真的补下缺的章节。
func TestQueuePlanDetectsIncrementalGap(t *testing.T) {
	st := newTestStore(t)
	w := newWorkerForTest(st)

	mustAddFav(t, st, "jm", "G1", "缺新章的书")
	mustCache(t, st, "jm", "G1", "1")
	mustSetChapterList(t, st, "jm", "G1", "1", "2", "3")

	plan, err := w.QueueFavoritesDetailed(true, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Candidates != 1 {
		t.Fatalf("candidates = %d, want 1 (partial must not be skipped)", plan.Candidates)
	}
	if plan.Incremental != 1 {
		t.Fatalf("incremental = %d, want 1", plan.Incremental)
	}
	if plan.AlreadyCached != 0 {
		t.Fatalf("alreadyCached = %d, want 0", plan.AlreadyCached)
	}
	if len(plan.Items) != 1 || plan.Items[0].Missing != 2 {
		t.Fatalf("items = %+v, want missing=2", plan.Items)
	}

	// 执行：只补 2、3 两章
	plan2, err := w.QueueFavoritesDetailed(false, "favorite-backfill")
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Queued != 1 {
		t.Fatalf("queued = %d, want 1", plan2.Queued)
	}
	jobs, _, err := st.ListJobs("", 10, 0)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v / %v", jobs, err)
	}
	if len(jobs[0].Chapters) != 2 || jobs[0].Chapters[0] != "2" {
		t.Fatalf("job chapters = %v, want the 2 missing ones", jobs[0].Chapters)
	}
}
