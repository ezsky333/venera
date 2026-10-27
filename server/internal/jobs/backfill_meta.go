package jobs

import (
	"fmt"
	"sync"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// MetaDetailLoader 是补全元数据所需的最小依赖面。
//
// 只依赖"按 (source, comicId) 问源站要一次详情"，不依赖下载器，
// 这样面板与 App 都能触发同一个实现，也不会把下载队列牵扯进来。
type MetaDetailLoader interface {
	LoadDetail(key, comicID string) (*model.ComicDetail, error)
}

// MetaBackfillResult 是一次补全的统计。
type MetaBackfillResult struct {
	// Total 是本次扫描到的"元数据不全"条目数（可能被 limit 截断）。
	Total int `json:"total"`
	// Attempted 是真正发起了源站请求的数量。
	Attempted int `json:"attempted"`
	// Updated 是成功写回新元数据的数量。
	Updated int `json:"updated"`
	// Failed 是源站请求失败（未登录、站点不可达、源已删除…）的数量。
	Failed int `json:"failed"`
	// Skipped 是跳过（源未安装、源站也没给更多信息）的数量。
	Skipped int `json:"skipped"`
	// Remaining 是补全后仍然缺元数据的条目数。
	Remaining int `json:"remaining"`
	// Errors 是前若干条失败原因，供面板直接展示。
	Errors []string `json:"errors,omitempty"`
}

// MetaBackfiller 用源 JS 的 loadInfo 把 comics 表里缺的元数据补回来。
//
// 为什么需要单独一个入口：comics 表的 author/description 只在
// **下载任务**里写（downloader.RunJob → LoadDetail），而绝大多数条目
// 只经过收藏/历史同步（op 里没有 author/description）。
// 结果就是"ehentai 117 本全部没有作者和简介"这种存量缺口：
// 数据在源站上取得到，本地却永远是空的。
//
// 语义刻意做成"只补缺失、不覆盖已有"：
//   - 已有的 author/description 可能是用户手工改过的，不能被源站顶掉；
//   - 只对"缺字段"的条目发请求，成本和风险都最小。
type MetaBackfiller struct {
	st     *store.Store
	loader MetaDetailLoader
	logf   func(string, ...any)
	mu     sync.Mutex
}

// NewMetaBackfiller 构造补全器。
//
// loader 通常传 *source.Manager（它有 LoadDetail）。
func NewMetaBackfiller(st *store.Store, loader MetaDetailLoader, logf func(string, ...any)) *MetaBackfiller {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &MetaBackfiller{st: st, loader: loader, logf: logf}
}

// SetLoader 允许后置注入源管理器（构造顺序上 store 先于 source）。
func (b *MetaBackfiller) SetLoader(l MetaDetailLoader) { b.loader = l }

// Remaining 返回当前仍缺元数据的条目数（面板按钮上的角标）。
func (b *MetaBackfiller) Remaining() (int, error) {
	return b.st.CountComicsMissingMeta()
}

// Run 扫描并补全。
//
// [src] 非空时只处理该源；[limit] <= 0 时不限数量。
// 并发度刻意保持 1：源站多数有速率限制，多线程反而更容易被 429/屏蔽，
// 而补全是一次性的后台任务，快慢几秒对用户没有影响。
func (b *MetaBackfiller) Run(src string, limit int) (*MetaBackfillResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	targets, err := b.st.ComicsMissingMeta(src, limit)
	if err != nil {
		return nil, err
	}
	res := &MetaBackfillResult{Total: len(targets)}

	seen := make([]string, 0, 5)
	recordErr := func(format string, args ...any) {
		if len(seen) < 5 {
			seen = append(seen, fmt.Sprintf(format, args...))
		}
	}

	for _, t := range targets {
		if t.Source == "" || t.ComicID == "" || b.loader == nil {
			res.Skipped++
			continue
		}
		res.Attempted++
		detail, err := b.loader.LoadDetail(t.Source, t.ComicID)
		if err != nil {
			res.Failed++
			b.logf("backfill meta failed: %s/%s: %v", t.Source, t.ComicID, err)
			recordErr("%s/%s: %v", t.Source, t.ComicID, err)
			continue
		}
		if detail == nil {
			res.Failed++
			continue
		}

		// 只写"源站确实给了、而本地确实没有"的字段。
		// 已有 author 时绝不覆盖。
		//
		// 注意 ComicDetail 自己还有一个 Description 字段，它会**遮蔽**
		// 内嵌 model.Comic 的同名字段（Go 的字段提升规则）。
		// 两个都要看：loadInfo 的解析结果写在哪个字段取决于调用方。
		author := detail.Author
		if t.Author != "" {
			author = t.Author
		}
		desc := firstNonEmpty(detail.Description, detail.Comic.Description)
		uploader := detail.Uploader
		if author == "" && uploader == "" && desc == "" &&
			len(detail.Tags) == 0 && detail.Cover == "" {
			// 源站也没给更多信息（很多源不提供 author/description）。
			res.Skipped++
			continue
		}

		if err := b.st.UpsertComic(model.Comic{
			Source:      t.Source,
			ID:          t.ComicID,
			Title:       firstNonEmpty(detail.Title, t.Title),
			Subtitle:    detail.Subtitle,
			Cover:       detail.Cover,
			Tags:        detail.Tags,
			Author:      author,
			Uploader:    uploader,
			Description: desc,
		}, 0); err != nil {
			res.Failed++
			b.logf("backfill meta save failed: %s/%s: %v", t.Source, t.ComicID, err)
			recordErr("%s/%s: %v", t.Source, t.ComicID, err)
			continue
		}
		res.Updated++
	}

	if n, err := b.st.CountComicsMissingMeta(); err == nil {
		res.Remaining = n
	}
	res.Errors = seen
	return res, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
