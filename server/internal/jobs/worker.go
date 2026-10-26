package jobs

import (
	"context"
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
	}
	for _, p := range retriable {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// Enqueue 为一个漫画排队下载，带去重。
func (w *Worker) Enqueue(source, comicID, title, origin string, chapters []string) (int64, error) {
	if exists, err := w.st.PendingJobExists(source, comicID); err == nil && exists {
		return 0, ErrAlreadyQueued
	}
	return w.st.CreateJob(&model.Job{
		Source: source, ComicID: comicID, Title: title,
		Chapters: chapters, Origin: origin,
	})
}

// ErrAlreadyQueued 表示同一漫画已有待执行任务。
var ErrAlreadyQueued = fmt.Errorf("already queued")

// QueueFavorites 把收藏的漫画批量排队（跳过已缓存的）。
func (w *Worker) QueueFavorites() (queued int, err error) {
	favs, err := w.st.AllFavoriteComics()
	if err != nil {
		return 0, err
	}
	for _, f := range favs {
		if exists, _ := w.st.PendingJobExists(f.Source, f.ID); exists {
			continue
		}
		if _, err := w.st.CreateJob(&model.Job{
			Source: f.Source, ComicID: f.ID, Title: f.Title, Origin: "favorite-sync",
		}); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}
