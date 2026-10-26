package model

import "time"

// Comic 是服务端对一部漫画的抽象标识。
// (Source, ID) 是全局唯一键，等价于 App 的 (sourceKey, comicId)。
type Comic struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Cover  string `json:"cover,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	// MaxPage 仅在无章节的漫画（单本）中有意义。
	MaxPage *int `json:"maxPage,omitempty"`
}

// Key 返回 App 侧用来识别该漫画的复合键。
func (c Comic) Key() string { return c.Source + "\x00" + c.ID }

// Chapter 是一章（或一话）。
type Chapter struct {
	// ID 是源内部的章节标识，会作为章节目录名的一部分。
	ID    string `json:"id"`
	Title string `json:"title"`
	// Group 是漫画若带章节分组时的组名，空表示未分组。
	Group string `json:"group,omitempty"`
}

// ComicDetail 是详情页需要的完整信息，由源 JS 的 comic.loadInfo 产出。
type ComicDetail struct {
	Comic
	Description string    `json:"description,omitempty"`
	Chapters    []Chapter `json:"chapters,omitempty"`
	// Grouped 表示章节是否按组划分（ComicChapters.isGrouped）。
	Grouped bool `json:"grouped"`
}

// Favorite 是一条收藏记录。Folder 对应 App 的收藏夹名。
type Favorite struct {
	Comic
	Folder  string    `json:"folder"`
	Author  string    `json:"author,omitempty"`
	AddedAt time.Time `json:"addedAt"`
	// SourceType 是 App 中使用的 sourceKey.hashCode，用于与 history/comics 表对齐。
	SourceType int32 `json:"sourceType"`
}

// History 是一条阅读进度。
type History struct {
	Comic
	SourceType int32     `json:"sourceType"`
	Ep         int       `json:"ep"`
	Page       int       `json:"page"`
	Group      int       `json:"group"`
	ReadEps    []string  `json:"readEpisodes,omitempty"`
	MaxPage    *int      `json:"maxPage,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// CachedChapter 描述服务端已完整缓存的某一章。
type CachedChapter struct {
	Source    string    `json:"source"`
	ComicID   string    `json:"comicId"`
	ChapterID string    `json:"chapterId"`
	ChapterTitle string `json:"chapterTitle,omitempty"`
	// Dir 是相对 LibraryDir 的目录路径，客户端据此拼接媒体 URL。
	Dir       string    `json:"dir"`
	PageCount int       `json:"pageCount"`
	// Cover 是相对 LibraryDir 的封面路径（可能为空）。
	Cover     string    `json:"cover,omitempty"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"createdAt"`
}

// JobStatus 是下载任务状态。
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobDone      JobStatus = "done"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Job 是一个下载任务。粒度是"一部漫画的整体或指定章节"。
type Job struct {
	ID      int64     `json:"id"`
	Source  string    `json:"source"`
	ComicID string    `json:"comicId"`
	Title   string    `json:"title,omitempty"`
	// Chapters 为空表示"全部章节"。
	Chapters []string `json:"chapters,omitempty"`
	Status   JobStatus `json:"status"`
	Progress float64   `json:"progress"`
	Message  string    `json:"message,omitempty"`
	Error    string    `json:"error,omitempty"`
	// Origin 记录任务来源: manual / favorite-sync / follow
	Origin string `json:"origin,omitempty"`
	// Attempts 已执行次数，用于限制自动重试（0 表示从未执行）。
	Attempts  int       `json:"attempts,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// OpType 是同步操作类型。W2 协议是操作日志(operation log)模型：
// 每个客户端上传自己的操作，服务端线性化后广播给其他客户端。
type OpType string

const (
	OpFavoriteAdd    OpType = "fav.add"
	OpFavoriteRemove OpType = "fav.remove"
	OpFavoriteMove   OpType = "fav.move"
	OpHistoryUpdate  OpType = "history.update"
	OpSourceData     OpType = "source.data"
	OpSourceCookie   OpType = "source.cookie"
	OpSourceScript   OpType = "source.script"
	OpSourceRemove   OpType = "source.remove"
	OpSourceInstall  OpType = "source.install"
	OpSettingUpdate  OpType = "setting.update"
)

// Op 是一条同步操作。ID 由服务端分配，单调递增，作为客户端拉取的游标。
type Op struct {
	ID       int64           `json:"id"`
	ClientID string          `json:"clientId"`
	Type     OpType          `json:"type"`
	// Lamport 用于同类操作之间的因果排序（Lamport 时钟）。
	Lamport int64           `json:"lamport"`
	Payload any             `json:"payload"`
	// Origin 是产生该操作的设备 ID，用于回显抑制。
	At       time.Time       `json:"at"`
}

// FavoriteOpPayload 是收藏增删的载荷。
type FavoriteOpPayload struct {
	Folder     string   `json:"folder"`
	Source     string   `json:"source"`
	ComicID    string   `json:"comicId"`
	Title      string   `json:"title,omitempty"`
	Author     string   `json:"author,omitempty"`
	Cover      string   `json:"cover,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	SourceType int32    `json:"sourceType,omitempty"`
}

// FavoriteMovePayload 是收藏夹间移动的载荷。
type FavoriteMovePayload struct {
	FromFolder string   `json:"fromFolder"`
	ToFolder   string   `json:"toFolder"`
	Source     string   `json:"source"`
	ComicID    string   `json:"comicId"`
	Title      string   `json:"title,omitempty"`
	Author     string   `json:"author,omitempty"`
	Cover      string   `json:"cover,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	SourceType int32    `json:"sourceType,omitempty"`
}

// HistoryOpPayload 是阅读进度的载荷。
type HistoryOpPayload struct {
	Source     string   `json:"source"`
	ComicID    string   `json:"comicId"`
	Title      string   `json:"title,omitempty"`
	Cover      string   `json:"cover,omitempty"`
	SourceType int32    `json:"sourceType,omitempty"`
	Ep         int      `json:"ep"`
	Page       int      `json:"page"`
	Group      int      `json:"group"`
	ReadEps    []string `json:"readEpisodes,omitempty"`
	MaxPage    *int     `json:"maxPage,omitempty"`
	// At 是客户端的操作时间，用于 Last-Write-Wins。
	At int64 `json:"at"`
}

// SourceDataOpPayload 承载漫画源的私有数据（登录态、token 等）。
type SourceDataOpPayload struct {
	Source string         `json:"source"`
	Data   map[string]any `json:"data"`
}

// SourceScriptOpPayload 承载源的脚本本身。
//
// 设计要点（对应"只同步已安装的源"这个需求）：
//   - 客户端装了什么源，就同步什么源；没装的不应该出现在服务端。
//   - 服务端只持有被同步过来的源，下载队列自然也只会涉及这些源。
//
// 这与"全量下发 venera-configs 里所有源"是两种不同的语义：
// 后者会让服务端和 pad 上都凭空多出几十个用户根本没启用的源。
type SourceScriptOpPayload struct {
	Source  string `json:"source"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	// Script 是完整的 JS 源码。
	Script string `json:"script"`
}

// SourceRemoveOpPayload 表示客户端卸载了一个源，服务端应当同步移除。
type SourceRemoveOpPayload struct {
	Source string `json:"source"`
}

// SourceInstallOpPayload 表示客户端装好了一个源（仅元信息）。
//
// 用于"服务端已经有这个源、客户端刚装上"的场景：客户端只需上报
// key/name/version，脚本可以通过 /api/v1/sources/{key}/script 取回，
// 避免把几十 KB 的脚本重复塞进 op 日志。
type SourceInstallOpPayload struct {
	Source   string `json:"source"`
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	HasLocal bool   `json:"hasLocal"`
}

// SourceCookieOpPayload 承载某个源的 Cookie 集合。
type SourceCookieOpPayload struct {
	Source  string         `json:"source"`
	Cookies []CookieRecord `json:"cookies"`
}

// CookieRecord 是 Cookie 的可序列化形式，字段对齐 App 的 cookie.db。
type CookieRecord struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path,omitempty"`
	Expires  *int64 `json:"expires,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
}

// SettingOpPayload 承载普通配置项变更。
type SettingOpPayload struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// MediaItem 是客户端可播放/可显示的单页图片。
type MediaItem struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
	Bytes int64  `json:"bytes,omitempty"`
}
