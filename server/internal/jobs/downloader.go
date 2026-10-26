// Package jobs 实现下载任务引擎：从源取章节与图片、按 App 的目录布局落盘、
// 并记录可被客户端查询的缓存索引。
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// SourceRunner 是下载器对源管理器的依赖（便于测试替换）。
type SourceRunner interface {
	LoadDetail(sourceKey, comicID string) (*model.ComicDetail, error)
	LoadEpisodes(sourceKey, comicID, ep string) ([]string, error)
	ImageConfigFor(sourceKey, imageURL, comicID, ep string) (*ImageConfig, error)
}

// ImageConfig 与 source.ImageConfig 对应（避免循环依赖）。
type ImageConfig struct {
	URL           string
	Method        string
	Data          any
	Headers       map[string]string
	OnResponseJS  string
	ModifyImageJS string
}

// Downloader 把一部漫画（或其中若干章）下载到库目录。
type Downloader struct {
	store *store.Store
	run   SourceRunner
	// concurrency 是单章内图片并发数。
	concurrency int
	// transform 负责 onResponse / modifyImage（可为 nil）。
	transform TransformFunc
	// fetch 负责实际下载图片（可为 nil，使用默认实现）。
	fetch FetchFunc
	// logger
	logf func(format string, args ...any)
}

// TransformFunc 对图片字节做源要求的变换。
//
// initJS 是 venera 的 JS 桥（init.js）。modifyImage 脚本依赖它定义的
// Image/ComicSource 等全局，因此必须一并传入。
type TransformFunc func(initJS string, cfg *ImageConfig, data []byte) ([]byte, error)

// FetchFunc 下载单张图片。
type FetchFunc func(url string, headers map[string]string) ([]byte, error)

// NewDownloader 创建下载器。
func NewDownloader(st *store.Store, run SourceRunner, concurrency int) *Downloader {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Downloader{
		store:       st,
		run:         run,
		concurrency: concurrency,
		logf:        func(string, ...any) {},
	}
}

// SetLogger 注入日志函数。
func (d *Downloader) SetLogger(f func(format string, args ...any)) { d.logf = f }

// SetTransform 注入图片变换实现。
func (d *Downloader) SetTransform(f TransformFunc) { d.transform = f }

// SetFetch 注入图片下载实现。
func (d *Downloader) SetFetch(f FetchFunc) { d.fetch = f }

// RunJob 执行一个下载任务。
//
// 目录布局与 App 的 LocalManager 保持一致：
//
//	<library>/<漫画目录名>/cover.png
//	<library>/<漫画目录名>/<章节目录名>/0.png 1.png ...
//
// 这样客户端的"本地阅读"逻辑可以零改动复用。
func (d *Downloader) RunJob(jobID int64, detail *model.ComicDetail) error {
	if detail == nil {
		return fmt.Errorf("nil comic detail")
	}
	job, err := d.store.GetJob(jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// 决定要下载哪些章节
	targets := detail.Chapters
	if len(job.Chapters) > 0 {
		want := map[string]bool{}
		for _, c := range job.Chapters {
			want[c] = true
		}
		var filtered []model.Chapter
		for _, c := range targets {
			if want[c.ID] {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("none of the requested chapters exist")
		}
		targets = filtered
	}

	// 把**完整**章节列表落库（不是 targets，是源端全量）。
	//
	// 这是离线兜底详情页能正确还原 history.ep 的前提：
	// 客户端只按"第 N 项"定位章节，若服务端只回已缓存的章节，
	// 下标就会整体前移，打开的就是错的章节。
	if err := d.store.SetComicChapterList(detail.Source, detail.ID, detail.Chapters); err != nil {
		d.logf("save chapter list failed for %s: %v", detail.ID, err)
	}

	comicDir := comicDirName(detail.Title, detail.ID)
	baseDir := filepath.Join(d.store.LibraryDir(), comicDir)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("create comic dir: %w", err)
	}

	// 封面
	if detail.Cover != "" {
		if err := d.saveCover(detail, baseDir); err != nil {
			d.logf("cover download failed for %s: %v", detail.ID, err)
		}
	}

	// 已缓存的章节跳过
	cachedSet := map[string]bool{}
	if list, err := d.store.ListCachedByComic(detail.Source, detail.ID); err == nil {
		for _, c := range list {
			cachedSet[c.ChapterID] = true
		}
	}

	total := len(targets)
	var failedChapters []string
	var lastErr error
	for i, ch := range targets {
		if cachedSet[ch.ID] {
			d.logf("[%d/%d] skip cached chapter %s", i+1, total, ch.ID)
			continue
		}
		progress := float64(i) / float64(total)
		_ = d.store.UpdateJob(jobID, model.JobRunning, progress,
			fmt.Sprintf("downloading chapter %d/%d: %s", i+1, total, ch.Title), "")

		// 单章失败不再中止整本。
		//
		// 一本 200 话的漫画若在第 3 话因为一次 EOF 就整体失败，
		// 那么每次重试都要从头再来，永远也下载不完。
		// 正确做法是记下失败章节继续往下下，最后按比例判定整本状态。
		if err := d.downloadChapter(detail, ch, baseDir); err != nil {
			d.logf("[%d/%d] chapter %s failed: %v", i+1, total, ch.ID, err)
			failedChapters = append(failedChapters, ch.ID)
			lastErr = err
		}
	}

	// 一章都没成功 → 整本失败（多半是源配置/登录问题，重试有意义）
	if len(failedChapters) > 0 && len(failedChapters) >= len(targets) {
		msg := fmt.Sprintf("all %d chapters failed (last: %v)", len(failedChapters), lastErr)
		_ = d.store.UpdateJob(jobID, model.JobFailed, 0, "", msg)
		return fmt.Errorf("%s", msg)
	}
	if len(failedChapters) > 0 {
		// 部分成功：标记完成，但在 message 里留痕，便于管理面板展示
		msg := fmt.Sprintf("complete with %d/%d chapters failed", len(failedChapters), len(targets))
		d.logf("job %d: %s", jobID, msg)
		_ = d.store.UpdateJob(jobID, model.JobDone, 1.0, msg, "")
		return nil
	}

	_ = d.store.UpdateJob(jobID, model.JobDone, 1.0, "complete", "")
	return nil
}

// saveCover 下载封面到漫画目录。
func (d *Downloader) saveCover(detail *model.ComicDetail, baseDir string) error {
	cfg, err := d.run.ImageConfigFor(detail.Source, detail.Cover, detail.ID, "")
	if err != nil {
		cfg = &ImageConfig{}
	}
	url := detail.Cover
	headers := map[string]string{}
	if cfg != nil {
		if cfg.URL != "" {
			url = cfg.URL
		}
		headers = cfg.Headers
	}

	// 封面同样会被上游抖动影响；失败三次就放弃（封面缺失不影响阅读）
	var data []byte
	var lastErr error
	for i := 0; i < 3; i++ {
		data, lastErr = d.fetchImage(url, headers)
		if lastErr == nil {
			break
		}
		if !isRetriableImageError(lastErr.Error()) {
			return lastErr
		}
		time.Sleep(time.Duration(300*(i+1)) * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	ext := imageExt(data)
	path := filepath.Join(baseDir, "cover"+ext)
	return os.WriteFile(path, data, 0o644)
}

// downloadChapter 下载一章的所有图片。
func (d *Downloader) downloadChapter(detail *model.ComicDetail, ch model.Chapter, baseDir string) error {
	urls, err := d.run.LoadEpisodes(detail.Source, detail.ID, ch.ID)
	if err != nil {
		return fmt.Errorf("load pages: %w", err)
	}
	if len(urls) == 0 {
		// 空章节（例如外链章节）视为成功但记录 0 页，避免阻塞整本
		return d.store.PutCachedChapter(model.CachedChapter{
			Source: detail.Source, ComicID: detail.ID, ChapterID: ch.ID,
			ChapterTitle: ch.Title,
			Dir:          relativeTo(d.store.LibraryDir(), filepath.Join(baseDir, chapterDirName(ch))),
			PageCount:    0, CreatedAt: time.Now(),
		})
	}

	dirName := chapterDirName(ch)
	dir := filepath.Join(baseDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// 章节级图片加载配置（部分源对同章所有图使用同一配置）
	cfgCache := sync.Map{}

	type result struct {
		index int
		err   error
	}
	results := make(chan result, len(urls))

	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup

	for idx, imgURL := range urls {
		wg.Add(1)
		go func(idx int, imgURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			data, err := d.fetchImageWithRetry(detail, ch, imgURL, &cfgCache)
			if err != nil {
				results <- result{idx, err}
				return
			}
			path := filepath.Join(dir, fmt.Sprintf("%d%s", idx, imageExt(data)))
			if err := os.WriteFile(path, data, 0o644); err != nil {
				results <- result{idx, err}
				return
			}
			results <- result{idx, nil}
		}(idx, imgURL)
	}
	wg.Wait()
	close(results)

	// 收敛错误。注意这里**不**因为个别图片失败而放弃整章：
	// 上游图片 CDN 抖动很常见（picacg 一次 200 页里挂 3 张是常态），
	// 让整本 200 页白下载再全部重来，代价远高于容忍少量缺页。
	// 缺页数量记在章节索引里，客户端遇到缺页会回落到在线源。
	var firstErr error
	var failedIdx []int
	for r := range results {
		if r.err != nil {
			failedIdx = append(failedIdx, r.index)
			if firstErr == nil {
				firstErr = r.err
			}
		}
	}
	total := len(urls)
	if len(failedIdx) > 0 {
		d.logf("chapter %s: %d/%d images failed (first: %v)", ch.ID, len(failedIdx), total, firstErr)
	}
	// 超过容忍阈值才判定失败，避免把"几乎全没下到"当成成功
	if len(failedIdx) > maxFailedImages(total) {
		return fmt.Errorf("%d/%d images failed (first: %v)", len(failedIdx), total, firstErr)
	}

	// 统计大小
	var totalBytes int64
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				totalBytes += info.Size()
			}
		}
	}

	rel, _ := filepath.Rel(d.store.LibraryDir(), dir)
	pages := total - len(failedIdx)
	return d.store.PutCachedChapter(model.CachedChapter{
		Source:       detail.Source,
		ComicID:      detail.ID,
		ChapterID:    ch.ID,
		ChapterTitle: ch.Title,
		Dir:          filepath.ToSlash(rel),
		PageCount:    pages,
		Cover:        relativeTo(d.store.LibraryDir(), filepath.Join(baseDir, "cover.png")),
		Bytes:        totalBytes,
		CreatedAt:    time.Now(),
	})
}

// maxFailedImages 是单章可容忍的缺页数。
//
// 取"总页数的 10%，且至少 2 页"，理由：
//   - 图片 CDN 偶发单张失败不可避免，为此重下整本不划算；
//   - 但缺页过多说明源/网络确实有问题，应让任务失败以便重试。
func maxFailedImages(total int) int {
	if total <= 0 {
		return 0
	}
	n := total / 10
	if n < 2 {
		n = 2
	}
	return n
}

// fetchImageWithRetry 下载单张图片，对可重试错误做有限次退避重试。
//
// 实测：picacg 的 storage-b 与 jm 的几个 cdn 域会零星返回 EOF /
// 空 body / HTML 错误页，隔几百毫秒重试基本都能成功。
func (d *Downloader) fetchImageWithRetry(detail *model.ComicDetail, ch model.Chapter, imgURL string, cache *sync.Map) ([]byte, error) {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		data, err := d.fetchWithConfig(detail, ch, imgURL, cache)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !isRetriableImageError(err.Error()) {
			return nil, err
		}
		// 0.4s / 0.8s 退避，避开上游瞬时抖动
		time.Sleep(time.Duration(400*(i+1)) * time.Millisecond)
	}
	return nil, lastErr
}

// isRetriableImageError 判断图片下载失败是否值得重试。
//
// 图片层面几乎全部是瞬时问题：连接重置、超时、空响应、
// 以及上游在限流时返回的 HTML 错误页（表现为解码失败）。
// 404/403 这类确定性失败不重试。
func isRetriableImageError(msg string) bool {
	permanent := []string{"status 404", "status 403", "status 401", "status 410"}
	for _, p := range permanent {
		if strings.Contains(msg, p) {
			return false
		}
	}
	return true
}

// fetchWithConfig 获取单张图片，按源的 ImageLoadingConfig 处理变换。
func (d *Downloader) fetchWithConfig(detail *model.ComicDetail, ch model.Chapter, imgURL string, cache *sync.Map) ([]byte, error) {
	var cfg *ImageConfig
	if v, ok := cache.Load(imgURL); ok {
		cfg, _ = v.(*ImageConfig)
	} else {
		c, err := d.run.ImageConfigFor(detail.Source, imgURL, detail.ID, ch.ID)
		if err != nil {
			c = &ImageConfig{}
		}
		cfg = c
		cache.Store(imgURL, cfg)
	}

	url := imgURL
	var headers map[string]string
	if cfg != nil {
		if cfg.URL != "" {
			url = cfg.URL
		}
		headers = cfg.Headers
	}
	data, err := d.fetchImage(url, headers)
	if err != nil {
		return nil, err
	}

	// onResponse（字节级）与 modifyImage（图片级）变换
	if cfg != nil && (cfg.OnResponseJS != "" || cfg.ModifyImageJS != "") && d.transform != nil {
		data, err = d.transform("", cfg, data)
		if err != nil {
			return nil, fmt.Errorf("transform: %w", err)
		}
	}
	return data, nil
}

func (d *Downloader) fetchImage(url string, headers map[string]string) ([]byte, error) {
	if d.fetch == nil {
		return nil, fmt.Errorf("no fetch function configured")
	}
	return d.fetch(url, headers)
}

// ComicDirName / ChapterDirName 暴露给外部（客户端需用同样规则定位目录）。

// ComicDirName 生成漫画目录名，与 App 的 findValidDirectoryName 规则接近：
// 保留可读标题，追加 id 短哈希保证唯一，并过滤非法字符。
func ComicDirName(title, comicID string) string { return comicDirName(title, comicID) }

func comicDirName(title, comicID string) string {
	t := sanitize(title)
	if t == "" {
		t = "comic"
	}
	if len([]rune(t)) > 60 {
		t = string([]rune(t)[:60])
	}
	// 短 id 后缀避免同名漫画互相覆盖
	short := shortID(comicID)
	return fmt.Sprintf("%s [%s]", t, short)
}

// ChapterDirName 生成章节目录名。
// 必须包含章节 ID 的一部分，因为章节标题可能重复或为空。
func ChapterDirName(ch model.Chapter) string { return chapterDirName(ch) }

func chapterDirName(ch model.Chapter) string {
	id := sanitize(ch.ID)
	if id == "" {
		id = "0"
	}
	// App 用 chapter id 作为目录名；这里限制长度并保持稳定
	if len([]rune(id)) > 80 {
		id = string([]rune(id)[:80])
	}
	return id
}

// sanitize 移除文件系统非法字符。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// shortID 取 id 的一小段用于目录名去重。
func shortID(id string) string {
	clean := sanitize(id)
	r := []rune(clean)
	if len(r) <= 8 {
		return clean
	}
	// 取前 8 个字符即可在绝大多数源中保持唯一
	return string(r[:8])
}

// imageExt 按魔数判断图片扩展名。
func imageExt(b []byte) string {
	if len(b) >= 12 {
		switch {
		case b[0] == 0xFF && b[1] == 0xD8:
			return ".jpg"
		case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
			return ".png"
		case b[0] == 'G' && b[1] == 'I' && b[2] == 'F':
			return ".gif"
		case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
			return ".webp"
		}
	}
	return ".jpg"
}

// relativeTo 计算相对路径（失败时返回原路径）。
func relativeTo(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

var _ = sort.Strings
