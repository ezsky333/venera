package store

import (
	"database/sql"
	"strings"
	"time"
)

// ---------- 管理面板用的聚合查询 ----------
//
// 这些查询只服务于 /admin 面板与 /api/v1/admin/* 接口，
// 与客户端阅读链路无关，因此单独放一个文件，避免污染核心查询。

// Stats 是管理面板顶部的总览。
type Stats struct {
	// 库
	LibraryBytes   int64 `json:"libraryBytes"`
	LibraryComics  int   `json:"libraryComics"`
	LibraryChapters int  `json:"libraryChapters"`
	LibraryPages   int   `json:"libraryPages"`
	// 收藏
	FavoriteFolders int `json:"favoriteFolders"`
	FavoriteItems   int `json:"favoriteItems"`
	// 同步
	Cursor       int64 `json:"cursor"`
	HistoryCount int   `json:"historyCount"`
	ClientCount  int   `json:"clientCount"`
	// 任务
	JobsPending int `json:"jobsPending"`
	JobsRunning int `json:"jobsRunning"`
	JobsDone    int `json:"jobsDone"`
	JobsFailed  int `json:"jobsFailed"`
	// 源
	SourceCount int `json:"sourceCount"`
}

// Stats 汇总面板需要的所有计数。
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}

	// 库统计
	row := s.db.QueryRow(`
		SELECT COALESCE(SUM(bytes),0), COUNT(DISTINCT source || '/' || comic_id),
		       COUNT(*), COALESCE(SUM(page_count),0)
		FROM cached_chapters`)
	if err := row.Scan(&st.LibraryBytes, &st.LibraryComics, &st.LibraryChapters, &st.LibraryPages); err != nil {
		return nil, err
	}

	// 收藏
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT folder), COUNT(*) FROM favorites`).
		Scan(&st.FavoriteFolders, &st.FavoriteItems); err != nil {
		return nil, err
	}

	// 同步
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM ops`).Scan(&st.Cursor); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM histories`).Scan(&st.HistoryCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM clients`).Scan(&st.ClientCount); err != nil {
		return nil, err
	}

	// 任务
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		switch status {
		case "pending":
			st.JobsPending = n
		case "running":
			st.JobsRunning = n
		case "done":
			st.JobsDone = n
		case "failed":
			st.JobsFailed = n
		}
	}

	// 源
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&st.SourceCount); err != nil {
		// sources 表可能为空（脚本从磁盘扫描而来），不是错误
		st.SourceCount = 0
	}
	return st, nil
}

// FolderStat 是一个收藏夹的概况。
type FolderStat struct {
	Folder string `json:"folder"`
	Count  int    `json:"count"`
	// Cached 是该收藏夹内已缓存到 NAS 的漫画数。
	Cached int `json:"cached"`
}

// FolderStats 按收藏夹统计（面板左侧列表）。
func (s *Store) FolderStats() ([]FolderStat, error) {
	rows, err := s.db.Query(`
		SELECT f.folder, COUNT(*) AS total,
		       COUNT(DISTINCT CASE WHEN cc.comic_id IS NOT NULL THEN f.source || '/' || f.comic_id END) AS cached
		FROM favorites f
		LEFT JOIN (
			SELECT source, comic_id FROM cached_chapters GROUP BY source, comic_id
		) cc ON cc.source = f.source AND cc.comic_id = f.comic_id
		GROUP BY f.folder
		ORDER BY total DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FolderStat
	for rows.Next() {
		var f FolderStat
		if err := rows.Scan(&f.Folder, &f.Count, &f.Cached); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// LibraryItem 是库里一本漫画的概况。
type LibraryItem struct {
	Source      string `json:"source"`
	ComicID     string `json:"comicId"`
	Title       string `json:"title"`
	Cover       string `json:"cover"`
	Chapters    int    `json:"chapters"`
	Pages       int    `json:"pages"`
	Bytes       int64  `json:"bytes"`
	UpdatedAt   string `json:"updatedAt"`
}

// ListLibrary 返回库中漫画（支持分页与按源过滤）。
func (s *Store) ListLibrary(source string, limit, offset int) ([]LibraryItem, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 60
	}
	where := ""
	args := []any{}
	if source != "" {
		where = " WHERE source = ?"
		args = append(args, source)
	}

	// 先取总数（面板分页需要）
	var total int
	countQ := `SELECT COUNT(DISTINCT source || '/' || comic_id) FROM cached_chapters` + where
	if err := s.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 注意：聚合结果要先读完再补查标题，否则会与同一连接互相等待
	// （SQLite 单写者模型下的典型案例）。
	q := `
		SELECT source, comic_id, COUNT(DISTINCT chapter_id), COALESCE(SUM(page_count),0),
		       COALESCE(SUM(bytes),0), COALESCE(MAX(created_at),0),
		       COALESCE(MAX(chapter_title),''), COALESCE(MAX(dir),'')
		FROM cached_chapters` + where + `
		GROUP BY source, comic_id
		ORDER BY MAX(created_at) DESC
		LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	var out []LibraryItem
	for rows.Next() {
		var it LibraryItem
		var created int64
		var dir string
		if err := rows.Scan(&it.Source, &it.ComicID, &it.Chapters, &it.Pages,
			&it.Bytes, &created, &it.Title, &dir); err != nil {
			rows.Close()
			return nil, 0, err
		}
		it.UpdatedAt = time.UnixMilli(created).Format(time.RFC3339)
		// 封面路径：cached_chapters 里存的是章节目录，
		// 用它推导出漫画目录再拼封面文件名（真正的扩展名在下面探测）。
		it.Cover = comicDirOf(dir)
		out = append(out, it)
	}
	rows.Close()

	// 用 comics 表里的权威标题覆盖（cached_chapters 的 title 是下载时的快照）
	for i := range out {
		if c, err := s.GetComic(out[i].Source, out[i].ComicID); err == nil && c != nil && c.Title != "" {
			out[i].Title = c.Title
		}
		if out[i].Title == "" {
			out[i].Title = out[i].ComicID
		}
	}
	return out, total, nil
}

// ChaptersOf 返回某漫画已缓存的章节（面板展开时用）。
func (s *Store) ChaptersOf(source, comicID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`
		SELECT chapter_id, COALESCE(chapter_title,''), dir, page_count, bytes, created_at
		FROM cached_chapters WHERE source = ? AND comic_id = ?
		ORDER BY chapter_id`, source, comicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, title, dir string
		var pages int
		var bytes int64
		var created int64
		if err := rows.Scan(&id, &title, &dir, &pages, &bytes, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"chapterId": id, "title": title, "dir": dir,
			"pages": pages, "bytes": bytes,
			"createdAt": time.UnixMilli(created).Format(time.RFC3339),
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out, rows.Err()
}

// DeleteComicCache 删除一本漫画的全部缓存（文件由调用方处理）。
func (s *Store) DeleteComicCache(source, comicID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT dir FROM cached_chapters WHERE source = ? AND comic_id = ?`, source, comicID)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		dirs = append(dirs, d)
	}
	rows.Close()

	if _, err := s.db.Exec(
		`DELETE FROM cached_chapters WHERE source = ? AND comic_id = ?`, source, comicID); err != nil {
		return nil, err
	}
	return dirs, nil
}

// sourceLibraryBytes 供 info 接口使用（保持与原 API 兼容）。
func (s *Store) SourceLibrary() (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT source, COALESCE(SUM(bytes),0) FROM cached_chapters GROUP BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// comicDirOf 从章节目录反推漫画目录。
//
// 章节目录形如 "标题 [短id]/1"，漫画目录是它的第一层。
func comicDirOf(chapterDir string) string {
	if chapterDir == "" {
		return ""
	}
	if i := strings.IndexByte(chapterDir, '/'); i > 0 {
		return chapterDir[:i]
	}
	return chapterDir
}

// CoverExtCandidates 是封面可能使用的扩展名（按落盘时的魔数决定）。
var CoverExtCandidates = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// ComicsWithChapters 返回所有"有至少一章已缓存"的漫画及其章节目录。
//
// 供封面补抓使用：这些漫画的内页已在盘上，但其中一部分可能因
// 封面下载失败（如 picacg 封面 301 跳 CDN 未跟随）而缺 cover 文件。
// coverDir 一并返回，调用方据此判断磁盘上是否已有 cover。
func (s *Store) ComicWithChapters() ([]map[string]any, error) {
	rows, err := s.db.Query(`
		SELECT c.source, c.comic_id, COALESCE(c.title,''),
		       COALESCE(c.cover,''), cc.dir
		FROM cached_chapters cc
		JOIN comics c
		  ON c.source = cc.source AND c.comic_id = cc.comic_id
		GROUP BY c.source, c.comic_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var src, cid, title, cover, dir string
		if err := rows.Scan(&src, &cid, &title, &cover, &dir); err != nil {
			return nil, err
		}
		// dir 形如 "标题 [id]/1"，第一层即漫画目录
		comicDir := dir
		if i := strings.IndexByte(comicDir, '/'); i > 0 {
			comicDir = comicDir[:i]
		}
		out = append(out, map[string]any{
			"source": src, "comicId": cid, "title": title,
			"cover": cover, "dir": comicDir,
		})
	}
	return out, rows.Err()
}

var _ = sql.ErrNoRows
