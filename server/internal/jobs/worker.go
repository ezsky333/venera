package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// Worker 驱动任务队列。它按配置的并发度领取 pending 任务并执行。
type Worker struct {
	st *store.Store
	dl *Downloader
	// run 用于取漫画详情（与 Downloader 共享同一源管理器）。
	run SourceRunner
	// concurrency 是同时执行的下载任务数（每个任务内部还有图片并发）。
	concurrency int
	// pollInterval 是队列空闲时的轮询间隔。
	pollInterval time.Duration
	logf         func(format string, args ...any)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorker 创建 worker。
func NewWorker(st *store.Store, run SourceRunner, concurrency int) *Worker {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &Worker{
		st:           st,
		run:          run,
		concurrency:  concurrency,
		pollInterval: 2 * time.Second,
		logf:         func(string, ...any) {},
	}
}

// SetDownloader 注入下载器。
func (w *Worker) SetDownloader(dl *Downloader) { w.dl = dl }

// SetLogger 注入日志。
func (w *Worker) SetLogger(f func(format string, args ...any)) { w.logf = f }

// Start 启动后台执行。
func (w *Worker) Start(ctx context.Context) {
	w.ctx, w.cancel = context.WithCancel(ctx)

	// 重启时把上次中断的任务放回队列
	if n, err := w.st.RequeueStaleJobs(); err == nil && n > 0 {
		w.logf("requeued %d stale jobs after restart", n)
	}

	for i := 0; i < w.concurrency; i++ {
		w.wg.Add(1)
		go func(id int) {
			defer w.wg.Done()
			w.loop(id)
		}(i)
	}
}

// Stop 停止并等待所有 worker 退出。
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func (w *Worker) loop(id int) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		job, err := w.st.ClaimNextJob()
		if err != nil {
			w.logf("worker %d: claim job: %v", id, err)
			w.sleep(ticker)
			continue
		}
		if job == nil {
			w.sleep(ticker)
			continue
		}

		w.logf("worker %d: running job %d (%s/%s)", id, job.ID, job.Source, job.ComicID)
		w.runJob(id, job)
	}
}

func (w *Worker) sleep(ticker *time.Ticker) {
	select {
	case <-w.ctx.Done():
	case <-ticker.C:
	}
}

func (w *Worker) runJob(workerID int, job *model.Job) {
	if w.dl == nil {
		_ = w.st.UpdateJob(job.ID, model.JobFailed, 0, "", "no downloader configured")
		return
	}

	// 取漫画详情（失败则标记任务失败，但不退出 worker）
	detail, err := w.run.LoadDetail(job.Source, job.ComicID)
	if err != nil {
		msg := fmt.Sprintf("load comic info: %v", err)

		// 情况一：登录态还没同步过来。
		//
		// 真机现场：客户端刚连上 NAS，收藏先落库 → 200 个任务立刻开跑，
		// 而 source_cookies 还是空的 → pica 全部 "Not logged in"、
		// ehentai 全部 302，全部被记成永久失败。等 Cookie 到了，
		// 这些任务已经死了，用户看到的是"连上了但一本都下不动"。
		//
		// 判据要两个条件同时成立才算"还没到"：
		//   1. 错误看起来是登录/权限类；
		//   2. 该源从来没收到过任何 source.data / source.cookie。
		// 第 2 条是关键——已经同步过凭证还登不上，那就是凭证真的失效了，
		// 这时候推迟只会让任务永远卡在队列里。
		if isAuthError(msg) && !w.st.SourceCredentialsKnown(job.Source) {
			if n, derr := w.st.DeferJob(job.ID, "waiting for client credentials"); derr == nil && n > 0 {
				w.logf("job %d deferred: source %q has no credentials synced yet (%s)",
					job.ID, job.Source, msg)
				return
			}
		}

		// 网络类抖动值得再试一次；配置/登录类错误重试只是浪费。
		if job.Attempts < maxJobAttempts && isRetriableError(msg) {
			if n, rerr := w.st.BumpJobAttempt(job.ID, msg); rerr == nil && n > 0 {
				w.logf("job %d retry %d/%d: %s", job.ID, job.Attempts+1, maxJobAttempts, msg)
				return
			}
		}

		w.logf("job %d failed: %s", job.ID, msg)
		_ = w.st.UpdateJob(job.ID, model.JobFailed, 0, "", msg)
		return
	}
	// 标题可能缺失，回填
	if job.Title == "" {
		job.Title = detail.Title
	}

	if err := w.dl.RunJob(job.ID, detail); err != nil {
		w.logf("job %d failed: %v", job.ID, err)
		return
	}
	w.logf("job %d done", job.ID)
}

// maxJobAttempts 是单个任务的最大执行次数（含首次）。
const maxJobAttempts = 2

// isAuthError 判断失败是否属于"登录态缺失/失效"这一类。
//
// 这类错误和网络抖动必须分开处理：
//   - 网络抖动 → 等一会儿重试有意义；
//   - 登录态缺失 → 重试一万次也没用，必须等客户端把 Cookie 同步上来。
//
// 各源的表现形式并不统一，实测见过的有：
//   - picacg：Error: Not logged in（源脚本自己 reLogin 抛的）
//   - ehentai/exhentai：Invalid status code: 302（未登录被重定向到登录页）
//   - 通用：401 / 403 / invalid login
func isAuthError(msg string) bool {
	auth := []string{
		"Not logged in",
		"not logged in",
		"not login",
		"invalid login",
		"login required",
		"unauthorized",
		"Invalid status code: 302",
		"Invalid Status Code 302",
		"Invalid status code: 401",
		"Invalid Status Code 401",
		"Invalid status code: 403",
		"Invalid Status Code 403",
		"may not have permission",
		"You may not have permission",
	}
	for _, p := range auth {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isRetriableError 判断失败是否属于"过一会儿再试可能就好了"。
//
// 只纳入网络/上游临时故障。像 invalid url、Custom domain is not set、
// 登录失效这类是配置问题，重试不会改变结果，重排只会占住 worker。
func isRetriableError(msg string) bool {
	permanent := []string{
		"invalid url:",
		"Custom domain is not set",
		"Setting not found",
		"js rejected: TypeError",
		"invalid login",
		"401",
		"403",
		// 需要登录才能看的页面：exhentai 的 302 就是这种情况。
		// 重试一万次也还是需要 Cookie，必须由客户端同步登录态来解决。
		"Invalid status code: 302",
		"Invalid status code: 404",
		"Invalid Status Code 302",
		"Invalid Status Code 404",
		"stopped after 10 redirects",
		"may not have permission",
		"You may not have permission",
	}
	for _, p := range permanent {
		if strings.Contains(msg, p) {
			return false
		}
	}
	retriable := []string{
		"timeout", "i/o timeout", "EOF", "connection reset",
		"429", "500", "502", "503", "504",
		"no such host", "TLS handshake",
		// 上游限流/维护时会返回 HTML 错误页，源里的 JSON.parse 于是报
		// "invalid character '<' looking for beginning of value"。
		// 这属于临时故障，等一下往往就好了。
		"invalid character '<'",
		"Unexpected token <",
	}
	for _, p := range retriable {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// Enqueue 为一个漫画排队下载，带去重。
//
// 两层去重：
//  1. 已有 pending/running 的同书任务 → ErrAlreadyQueued；
//  2. **库里已有缓存且无指定章节** → ErrAlreadyCached。
//
// 第 2 条是这次踩出来的：客户端每次启动都会 syncNow(full: true)，
// 把本地全部收藏重新推成 fav.add，服务端对每条新落库的收藏调用
// Enqueue。由于这里只查 "有没有待执行任务"，已经下完的书会被
// 反复重新排队——实测一次 App 重启就凭空多出 442 个 pending
// （picacg 222 / jm 60 / ehentai 18 / wnacg 21），把队列堵死。
//
// 注意：显式指定 chapters 时不算重复——那是用户手动"补下某几章"，
// 即使整本已缓存也应允许。
//
// 收藏自动同步走这条严格路径是正确的：它表达的是"这本我收藏了"，
// 不是"我要刷新这本"。用户显式点"推送到 NAS"要的是增量，
// 走 EnqueueIncremental。
func (w *Worker) Enqueue(source, comicID, title, origin string, chapters []string) (int64, error) {
	if exists, err := w.st.PendingJobExists(source, comicID); err == nil && exists {
		return 0, ErrAlreadyQueued
	}
	if len(chapters) == 0 {
		if cached, err := w.st.HasCachedChapters(source, comicID); err == nil && cached {
			return 0, ErrAlreadyCached
		}
	}
	return w.st.CreateJob(&model.Job{
		Source: source, ComicID: comicID, Title: title,
		Chapters: chapters, Origin: origin,
	})
}

// EnqueueResult 描述一次"增量入队"做了什么。
//
// 为什么要把这些数字回给客户端：用户点"推送到 NAS"时，可能是
//   - 首次下载（12 章全下）
//   - 只有 3 章新的（只下这 3 章）
//   - 已经是最新（一条都不该下）
//
// 三种情况的反馈必须不同，否则用户看到"已加入队列"却什么都没发生，
// 或者看到"已在 NAS 缓存"但其实漏了新章节。
type EnqueueResult struct {
	// JobID 新建的任务 id；0 表示没有入队（已是最新 / 已缓存）。
	JobID int64 `json:"jobId"`
	// Queued 是否真的创建了任务。
	Queued bool `json:"queued"`
	// Total 源端章节总数（0 = 章节表未知）。
	Total int `json:"total"`
	// Cached 服务端已缓存的章节数（按源端章节 id 计）。
	Cached int `json:"cached"`
	// Missing 本次要下载的章节 id（章节表未知时为空，表示"跑一遍看"）。
	Missing []string `json:"missing,omitempty"`
	// Reason 未入队的原因：up-to-date / already-queued。
	Reason string `json:"reason,omitempty"`
}

// EnqueueIncremental 以"补齐缺失章节"的语义入队。
//
// 这是用户显式动作（详情页「推送到 NAS」）该走的路径：
//
//   - 有源端章节表（comics.chapter_list）时，直接和已缓存集合求差集，
//     只把**新增章节**写进 job.chapters。这样一本 200 话的漫画更新了
//     3 话，只会重下这 3 话，而不是整本重来。
//   - 章节表未知（早期下载没记录）时，全量入队；worker 执行时
//     RunJob 里仍会按 cached_chapters 跳过已缓存章节，代价只是一次
//     loadInfo。
//   - 章节表已知且差集为空 → 返回 Queued=false / Reason=up-to-date，
//     不产生空任务行。
//
// 与 Enqueue 的关键区别就在第 1、3 条：Enqueue 只要发现"有任何缓存"
// 就拒绝，于是"已缓存 10 章、源站更新到 12 章"永远下不到后 2 章。
func (w *Worker) EnqueueIncremental(source, comicID, title, origin string, explicit []string) (*EnqueueResult, error) {
	if origin == "" {
		origin = "manual"
	}
	res := &EnqueueResult{}
	if exists, err := w.st.PendingJobExists(source, comicID); err == nil && exists {
		res.Reason = "already-queued"
		return res, ErrAlreadyQueued
	}

	// 显式章节：用户已经点名了要哪几章，不做差集。
	if len(explicit) > 0 {
		res.Missing = explicit
		id, err := w.st.CreateJob(&model.Job{
			Source: source, ComicID: comicID, Title: title,
			Chapters: explicit, Origin: origin,
		})
		if err != nil {
			return nil, err
		}
		res.JobID, res.Queued = id, true
		return res, nil
	}

	cachedSet, err := w.st.CachedChapterIDs(source, comicID)
	if err != nil {
		return nil, err
	}
	res.Cached = len(cachedSet)

	list, err := w.st.GetComicChapterList(source, comicID)
	if err != nil {
		return nil, err
	}

	if len(list) == 0 {
		// 章节表未知：分期情况。
		//
		//   - 有缓存 → 认为不需要重下（否则一次补录会把 126 本早期
		//     下载的漫画全部重跑一遍，实测库里就是这个数）；
		//   - 没缓存 → 全量入队。
		//
		// 这里之所以能直接用"有缓存"判定，是因为章节表为空本身就意味着
		// 这是画廊型源（源端 detail.Chapters 为空），整本只有一条记录。
		if len(cachedSet) > 0 {
			res.Reason = "up-to-date"
			return res, nil
		}
		id, err := w.st.CreateJob(&model.Job{
			Source: source, ComicID: comicID, Title: title,
			Chapters: nil, Origin: origin,
		})
		if err != nil {
			return nil, err
		}
		res.JobID, res.Queued = id, true
		return res, nil
	}

	// 章节表存在时**不**做"整本记录"判断。
	//
	// 曾经这里用 cachedSet[comicID] 当整本记录的标志，结果 jm 上踩雷：
	// jm 的漫画可以有章节 id 恰好等于漫画 id（实测 jm/330218 的章节列表
	// 里就有一章的 id 是 "330218"），于是整本被判成已缓存，
	// 真正缺的 3 章永远补不上。
	//
	// 正确判据是"章节表是否为空"：整本记录只会在源端没有章节列表时
	// 产生（见 Downloader.RunJob 里 targets 的兜底），而那种情况下
	// 上面的 len(list)==0 分支已经处理掉了。
	res.Total = len(list)
	var missing []string
	for _, ch := range list {
		if cachedSet[ch.ID] {
			continue
		}
		missing = append(missing, ch.ID)
	}
	if len(missing) == 0 {
		res.Reason = "up-to-date"
		return res, nil
	}

	res.Missing = missing
	id, err := w.st.CreateJob(&model.Job{
		Source: source, ComicID: comicID, Title: title,
		Chapters: missing, Origin: origin,
	})
	if err != nil {
		return nil, err
	}
	res.JobID, res.Queued = id, true
	return res, nil
}

// ErrAlreadyQueued 表示同一漫画已有待执行任务。
var ErrAlreadyQueued = fmt.Errorf("already queued")

// ErrAlreadyCached 表示该漫画已经完整缓存，无需重新下载。
var ErrAlreadyCached = fmt.Errorf("already cached")

// QueueFavorites 把收藏的漫画批量排队（跳过已缓存的）。
func (w *Worker) QueueFavorites() (queued int, err error) {
	plan, err := w.QueueFavoritesDetailed(false, "favorite-sync")
	if err != nil {
		return 0, err
	}
	return plan.Queued, nil
}

// QueuePlanItem 是预览里的一条。
type QueuePlanItem struct {
	Source  string `json:"source"`
	ComicID string `json:"comicId"`
	Title   string `json:"title"`
	// Missing 本章节新增/缺失的章节数；0 表示章节表未知（全量）。
	Missing int `json:"missing,omitempty"`
}

// QueuePlan 描述"补录未缓存收藏"这次会做什么。
//
// 分成规划（dryRun）和执行两步，是因为这两个数字差别极大：
// 447 本收藏里可能只有 8 本真没下过。没有预览的话，用户点一下
// 按钮不知道会往队列里塞 8 条还是 400 条，只能盲点。
type QueuePlan struct {
	// Candidates 是去重后、需要入队的漫画数（dryRun 时=将要入队的数量）。
	Candidates int `json:"candidates"`
	// Queued 是实际入队数（dryRun 时恒为 0）。
	Queued int `json:"queued"`
	// AlreadyQueued 已有 pending/running 任务，跳过。
	AlreadyQueued int `json:"alreadyQueued"`
	// AlreadyCached 库里已有**全部**章节，跳过。
	AlreadyCached int `json:"alreadyCached"`
	// Incremental 本次属于"已缓存一部分、只是缺新章节"的漫画数。
	//
	// 单独计数很有必要：这个数字不为 0 时，说明用户以前以为
	// "已经下全了"，其实源站后来更新过。旧实现会把这些书全部
	// 算进 AlreadyCached 直接跳过，缺口永远补不上。
	Incremental int `json:"incremental"`
	// Failed 入队过程出错的数量（单条失败不中断整批）。
	Failed int `json:"failed"`
	// BySource 待入队/已入队的漫画按源分布。
	BySource map[string]int `json:"bySource"`
	// Items 前若干条明细，供面板展示"到底哪些书会被下"。
	Items []QueuePlanItem `json:"items"`
	// DryRun 标记这份结果是预览还是执行结果。
	DryRun bool `json:"dryRun"`
}

// previewLimit 预览明细最多返回多少条（面板只看前几十条就够）。
const previewLimit = 50

// favoriteCoverage 判断一本收藏"还缺什么"。
//
// 三态：
//   - complete：章节表已知，且每一章都已缓存 → 无事可做；
//   - partial：有缓存但章节表里还有未缓存的章 → 需要增量补；
//   - empty：完全没缓存 → 需要全量下。
//
// 章节表未知（早期下载没记录 chapter_list）时退回"有缓存即 complete"，
// 否则一次补录会把 126 本早期漫画全部重跑一遍（实测库里就是这个数）。
func (w *Worker) favoriteCoverage(source, comicID string) (state string, missing int) {
	cachedSet, err := w.st.CachedChapterIDs(source, comicID)
	if err != nil {
		return "empty", 0
	}
	list, err := w.st.GetComicChapterList(source, comicID)
	if err != nil || len(list) == 0 {
		// 章节表未知（画廊型源，或早期下载没记录）：
		// 有缓存即视为完整，避免把存量整体重跑。
		if len(cachedSet) > 0 {
			return "complete", 0
		}
		return "empty", 0
	}
	n := 0
	for _, ch := range list {
		if !cachedSet[ch.ID] {
			n++
		}
	}
	if n == 0 {
		return "complete", 0
	}
	if len(cachedSet) > 0 {
		return "partial", n
	}
	return "empty", n
}

// QueueFavoritesDetailed 规划并（可选）执行"补录未缓存的收藏"。
//
// dryRun=true 时只统计、不写库；false 时按同一套判定实际入队。
// 两遍走的是同一段逻辑，避免"预览说 8 本、执行却入队 400 本"这种
// 预览与执行不一致的坑。
//
// origin 会写进 jobs.origin，用于区分这批任务是"收藏自动同步"还是
// "用户手动补录"——两者在面板上排查时含义完全不同。
func (w *Worker) QueueFavoritesDetailed(dryRun bool, origin string) (*QueuePlan, error) {
	if origin == "" {
		origin = "favorite-backfill"
	}
	favs, err := w.st.AllFavoriteComics()
	if err != nil {
		return nil, err
	}

	plan := &QueuePlan{BySource: map[string]int{}, DryRun: dryRun}

	// 收藏表里同一本漫画会出现在多个文件夹（实测 991 行 = 447 本），
	// 不去重会重复判定、也会让统计口径看起来像"要下 991 本"。
	seen := map[string]bool{}
	for _, f := range favs {
		key := f.Source + "\x00" + f.ID
		if seen[key] {
			continue
		}
		seen[key] = true

		if exists, _ := w.st.PendingJobExists(f.Source, f.ID); exists {
			plan.AlreadyQueued++
			continue
		}

		// 覆盖率判定：只有"章节表里每一章都已缓存"才算无事可做。
		state, missingCount := w.favoriteCoverage(f.Source, f.ID)
		if state == "complete" {
			plan.AlreadyCached++
			continue
		}

		if state == "partial" {
			plan.Incremental++
		}
		plan.Candidates++
		plan.BySource[f.Source]++
		if len(plan.Items) < previewLimit {
			plan.Items = append(plan.Items, QueuePlanItem{
				Source: f.Source, ComicID: f.ID, Title: f.Title,
				Missing: missingCount,
			})
		}

		if dryRun {
			continue
		}
		// 执行时复用 EnqueueIncremental：它会再算一次差集并把结果写进
		// job.chapters，从而只下缺的章节，不会整本重跑。
		r, err := w.EnqueueIncremental(f.Source, f.ID, f.Title, origin, nil)
		if err != nil {
			if errors.Is(err, ErrAlreadyQueued) {
				plan.AlreadyQueued++
				plan.Candidates--
				plan.BySource[f.Source]--
				continue
			}
			// 单条失败不中断整批：一本坏数据不该让剩下几百本都排不上
			plan.Failed++
			continue
		}
		if !r.Queued {
			plan.AlreadyCached++
			plan.Candidates--
			plan.BySource[f.Source]--
			continue
		}
		plan.Queued++
	}
	return plan, nil
}
