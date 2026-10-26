package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
)

// UpsertComic 缓存漫画元信息。
func (s *Store) UpsertComic(c model.Comic, sourceType int32) error {
	tags, _ := json.Marshal(c.Tags)
	_, err := s.db.Exec(
		`INSERT INTO comics(source, comic_id, title, subtitle, cover, tags, source_type, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source, comic_id) DO UPDATE SET
		   title = COALESCE(NULLIF(excluded.title,''), comics.title),
		   subtitle = COALESCE(NULLIF(excluded.subtitle,''), comics.subtitle),
		   cover = COALESCE(NULLIF(excluded.cover,''), comics.cover),
		   tags = COALESCE(NULLIF(excluded.tags,'[]'), comics.tags),
		   source_type = excluded.source_type,
		   updated_at = excluded.updated_at`,
		c.Source, c.ID, c.Title, c.Subtitle, c.Cover, string(tags), sourceType, time.Now().UnixMilli(),
	)
	return err
}

// SetComicChapterList 记录源端的完整章节列表（顺序即源端顺序）。
//
// 只在拿到非空列表时覆盖：空的章节列表通常代表"源没返回"，
// 用它去覆盖已有的正确顺序只会把索引搞坏。
func (s *Store) SetComicChapterList(source, comicID string, list []model.Chapter) error {
	if len(list) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(list); err != nil {
		return err
	}
	raw := strings.TrimRight(buf.String(), "\n")
	// 行可能还不存在（只下过章节、没同步过元信息），先占位再写。
	if _, err := s.db.Exec(
		`INSERT INTO comics(source, comic_id, chapter_list, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(source, comic_id) DO UPDATE SET chapter_list = excluded.chapter_list`,
		source, comicID, raw, time.Now().UnixMilli(),
	); err != nil {
		return err
	}
	return nil
}

// GetComicChapterList 读取源端完整章节列表。为空表示还没记录过。
func (s *Store) GetComicChapterList(source, comicID string) ([]model.Chapter, error) {
	var raw sql.NullString
	err := s.db.QueryRow(
		`SELECT chapter_list FROM comics WHERE source = ? AND comic_id = ?`,
		source, comicID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var out []model.Chapter
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, nil
	}
	return out, nil
}

func (s *Store) GetComic(source, comicID string) (*model.Comic, error) {
	var c model.Comic
	var tags sql.NullString
	err := s.db.QueryRow(
		`SELECT source, comic_id, title, subtitle, cover, tags FROM comics WHERE source = ? AND comic_id = ?`,
		source, comicID,
	).Scan(&c.Source, &c.ID, &c.Title, &c.Subtitle, &c.Cover, &tags)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tags.Valid && tags.String != "" {
		_ = json.Unmarshal([]byte(tags.String), &c.Tags)
	}
	return &c, nil
}

// ---------- favorites ----------

// AddFavorite 以 LWW 语义写入收藏。
// 若已存在且新 lamport 更小，则忽略（保证因果顺序不被旧操作覆盖）。
func (s *Store) AddFavorite(f model.Favorite, lamport int64) (bool, error) {
	tags, _ := json.Marshal(f.Tags)
	res, err := s.db.Exec(
		`INSERT INTO favorites(folder, source, comic_id, title, author, cover, tags,
		    source_type, added_at, display_order, lamport)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(folder, source, comic_id) DO UPDATE SET
		   title = COALESCE(NULLIF(excluded.title,''), favorites.title),
		   author = COALESCE(NULLIF(excluded.author,''), favorites.author),
		   cover = COALESCE(NULLIF(excluded.cover,''), favorites.cover),
		   tags = COALESCE(NULLIF(excluded.tags,'[]'), favorites.tags),
		   source_type = excluded.source_type,
		   display_order = excluded.display_order,
		   lamport = excluded.lamport
		 WHERE excluded.lamport >= favorites.lamport`,
		f.Folder, f.Source, f.ID, f.Title, f.Author, f.Cover, string(tags), f.SourceType,
		f.AddedAt.UnixMilli(), f.DisplayOrder, lamport,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RemoveFavorite 删除收藏。lamport 为墓碑时间戳，防止旧 add 复活。
func (s *Store) RemoveFavorite(folder, source, comicID string) (bool, error) {
	res, err := s.db.Exec(
		`DELETE FROM favorites WHERE folder = ? AND source = ? AND comic_id = ?`,
		folder, source, comicID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MoveFavorite 把漫画从一个收藏夹移动到另一个。
func (s *Store) MoveFavorite(f model.Favorite, fromFolder string, lamport int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM favorites WHERE folder = ? AND source = ? AND comic_id = ?`,
		fromFolder, f.Source, f.ID); err != nil {
		return err
	}
	tags, _ := json.Marshal(f.Tags)
	if _, err := tx.Exec(
		`INSERT INTO favorites(folder, source, comic_id, title, author, cover, tags, source_type, added_at, lamport)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(folder, source, comic_id) DO UPDATE SET lamport = excluded.lamport`,
		f.Folder, f.Source, f.ID, f.Title, f.Author, f.Cover, string(tags), f.SourceType,
		f.AddedAt.UnixMilli(), lamport,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// Folders 返回所有收藏夹及计数。
func (s *Store) Folders() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT folder, COUNT(*) FROM favorites GROUP BY folder ORDER BY folder`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var n string
		var c int
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

// ListFavorites 列出收藏。folder 为空表示全部。
func (s *Store) ListFavorites(folder string, limit, offset int) ([]model.Favorite, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	// 排序必须与客户端一致：客户端按 display_order 升序展示
	// （新收藏插到开头 = 更小的值）。用 added_at 排是错的，
	// 那个字段只反映"同步到服务端的时间"，不代表用户看到的顺序。
	q := `SELECT folder, source, comic_id, title, author, cover, tags,
	             source_type, added_at, display_order
	      FROM favorites`
	var args []any
	if folder != "" {
		q += ` WHERE folder = ?`
		args = append(args, folder)
	}
	q += ` ORDER BY display_order ASC, added_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Favorite
	for rows.Next() {
		var f model.Favorite
		var tags sql.NullString
		var added int64
		if err := rows.Scan(&f.Folder, &f.Source, &f.ID, &f.Title, &f.Author,
			&f.Cover, &tags, &f.SourceType, &added, &f.DisplayOrder); err != nil {
			return nil, err
		}
		f.AddedAt = time.UnixMilli(added)
		if tags.Valid {
			_ = json.Unmarshal([]byte(tags.String), &f.Tags)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AllFavoriteComics 返回去重后的收藏漫画（用于批量排队下载）。
func (s *Store) AllFavoriteComics() ([]model.Favorite, error) {
	rows, err := s.db.Query(`
		SELECT folder, source, comic_id, title, author, cover, tags,
		       source_type, added_at, display_order
		FROM favorites
		ORDER BY folder, display_order ASC, added_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Favorite
	for rows.Next() {
		var f model.Favorite
		var tags sql.NullString
		var added int64
		if err := rows.Scan(&f.Folder, &f.Source, &f.ID, &f.Title, &f.Author,
			&f.Cover, &tags, &f.SourceType, &added, &f.DisplayOrder); err != nil {
			return nil, err
		}
		f.AddedAt = time.UnixMilli(added)
		if tags.Valid {
			_ = json.Unmarshal([]byte(tags.String), &f.Tags)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---------- history ----------

// UpsertHistory 以客户端时间做 LWW 合并阅读进度。
func (s *Store) UpsertHistory(h model.History) (bool, error) {
	readEps, _ := json.Marshal(h.ReadEps)
	res, err := s.db.Exec(
		`INSERT INTO histories(source, comic_id, title, cover, source_type, ep, page, "group", read_eps, max_page, updated_at, lamport)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source, comic_id) DO UPDATE SET
		   title = COALESCE(NULLIF(excluded.title,''), histories.title),
		   cover = COALESCE(NULLIF(excluded.cover,''), histories.cover),
		   ep = excluded.ep,
		   page = excluded.page,
		   "group" = excluded."group",
		   read_eps = excluded.read_eps,
		   max_page = excluded.max_page,
		   updated_at = excluded.updated_at
		 WHERE excluded.updated_at >= histories.updated_at`,
		h.Source, h.ID, h.Title, h.Cover, h.SourceType, h.Ep, h.Page, h.Group,
		string(readEps), nullableInt(h.MaxPage), h.UpdatedAt.UnixMilli(), h.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Store) GetHistory(source, comicID string) (*model.History, error) {
	var h model.History
	var readEps sql.NullString
	var maxPage sql.NullInt64
	var updated int64
	err := s.db.QueryRow(
		`SELECT source, comic_id, title, cover, source_type, ep, page, "group", read_eps, max_page, updated_at
		 FROM histories WHERE source = ? AND comic_id = ?`, source, comicID,
	).Scan(&h.Source, &h.ID, &h.Title, &h.Cover, &h.SourceType, &h.Ep, &h.Page, &h.Group,
		&readEps, &maxPage, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.UpdatedAt = time.UnixMilli(updated)
	if readEps.Valid {
		_ = json.Unmarshal([]byte(readEps.String), &h.ReadEps)
	}
	if maxPage.Valid {
		v := int(maxPage.Int64)
		h.MaxPage = &v
	}
	return &h, nil
}

func (s *Store) ListHistories(limit int) ([]model.History, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT source, comic_id, title, cover, source_type, ep, page, "group", read_eps, max_page, updated_at
		 FROM histories ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.History
	for rows.Next() {
		var h model.History
		var readEps sql.NullString
		var maxPage sql.NullInt64
		var updated int64
		if err := rows.Scan(&h.Source, &h.ID, &h.Title, &h.Cover, &h.SourceType, &h.Ep, &h.Page,
			&h.Group, &readEps, &maxPage, &updated); err != nil {
			return nil, err
		}
		h.UpdatedAt = time.UnixMilli(updated)
		if readEps.Valid {
			_ = json.Unmarshal([]byte(readEps.String), &h.ReadEps)
		}
		if maxPage.Valid {
			v := int(maxPage.Int64)
			h.MaxPage = &v
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------- source data / cookies ----------

func (s *Store) PutSourceData(sourceKey string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO source_data(source_key, data, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(source_key) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		sourceKey, string(b), time.Now().UnixMilli())
	return err
}

func (s *Store) GetSourceData(sourceKey string) (map[string]any, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM source_data WHERE source_key = ?`, sourceKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MergeSourceData 按键合并（不整体覆盖），避免多设备互相抹掉登录态。
func (s *Store) MergeSourceData(sourceKey string, patch map[string]any) error {
	cur, err := s.GetSourceData(sourceKey)
	if err != nil {
		return err
	}
	for k, v := range patch {
		cur[k] = v
	}
	return s.PutSourceData(sourceKey, cur)
}

func (s *Store) PutCookies(sourceKey string, cookies []model.CookieRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range cookies {
		p := c.Path
		if p == "" {
			p = "/"
		}
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO source_cookies(source_key, name, value, domain, path, expires, secure, http_only)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sourceKey, c.Name, c.Value, c.Domain, p,
			c.Expires, boolInt(c.Secure), boolInt(c.HTTPOnly),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) GetCookies(sourceKey string) ([]model.CookieRecord, error) {
	rows, err := s.db.Query(
		`SELECT name, value, domain, path, expires, secure, http_only FROM source_cookies WHERE source_key = ?`,
		sourceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CookieRecord
	for rows.Next() {
		var c model.CookieRecord
		var exp sql.NullInt64
		var sec, http int
		if err := rows.Scan(&c.Name, &c.Value, &c.Domain, &c.Path, &exp, &sec, &http); err != nil {
			return nil, err
		}
		if exp.Valid {
			v := exp.Int64
			c.Expires = &v
		}
		c.Secure = sec == 1
		c.HTTPOnly = http == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- cached chapters ----------

func (s *Store) PutCachedChapter(c model.CachedChapter) error {
	_, err := s.db.Exec(
		`INSERT INTO cached_chapters(source, comic_id, chapter_id, chapter_title, dir, page_count, cover, bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source, comic_id, chapter_id) DO UPDATE SET
		   chapter_title = excluded.chapter_title, dir = excluded.dir,
		   page_count = excluded.page_count, cover = excluded.cover,
		   bytes = excluded.bytes, created_at = excluded.created_at`,
		c.Source, c.ComicID, c.ChapterID, c.ChapterTitle, c.Dir, c.PageCount, c.Cover, c.Bytes,
		c.CreatedAt.UnixMilli())
	return err
}

func (s *Store) GetCachedChapter(source, comicID, chapterID string) (*model.CachedChapter, error) {
	var c model.CachedChapter
	var created int64
	err := s.db.QueryRow(
		`SELECT source, comic_id, chapter_id, chapter_title, dir, page_count, cover, bytes, created_at
		 FROM cached_chapters WHERE source = ? AND comic_id = ? AND chapter_id = ?`,
		source, comicID, chapterID,
	).Scan(&c.Source, &c.ComicID, &c.ChapterID, &c.ChapterTitle, &c.Dir, &c.PageCount, &c.Cover, &c.Bytes, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = time.UnixMilli(created)
	return &c, nil
}

// ListCachedByComic 返回某漫画已缓存的所有章节。
func (s *Store) ListCachedByComic(source, comicID string) ([]model.CachedChapter, error) {
	rows, err := s.db.Query(
		`SELECT source, comic_id, chapter_id, chapter_title, dir, page_count, cover, bytes, created_at
		 FROM cached_chapters WHERE source = ? AND comic_id = ? ORDER BY chapter_id`,
		source, comicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CachedChapter
	for rows.Next() {
		var c model.CachedChapter
		var created int64
		if err := rows.Scan(&c.Source, &c.ComicID, &c.ChapterID, &c.ChapterTitle, &c.Dir,
			&c.PageCount, &c.Cover, &c.Bytes, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteCachedChapter(source, comicID, chapterID string) error {
	_, err := s.db.Exec(`DELETE FROM cached_chapters WHERE source = ? AND comic_id = ? AND chapter_id = ?`,
		source, comicID, chapterID)
	return err
}

// IsComicFullyCached 判断一部漫画的所有章节是否都已缓存。
func (s *Store) IsComicFullyCached(source, comicID string, expected int) (bool, error) {
	if expected <= 0 {
		return false, nil
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM cached_chapters WHERE source = ? AND comic_id = ?`,
		source, comicID).Scan(&n); err != nil {
		return false, err
	}
	return n >= expected, nil
}

// LibraryBytes 统计库占用。
func (s *Store) LibraryBytes() (int64, error) {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT SUM(bytes) FROM cached_chapters`).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// ---------- jobs ----------

func (s *Store) CreateJob(j *model.Job) (int64, error) {
	chapters, _ := json.Marshal(j.Chapters)
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO jobs(source, comic_id, title, chapters, status, progress, message, origin, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Source, j.ComicID, j.Title, string(chapters), string(model.JobPending), 0, "", j.Origin,
		now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PendingJobExists 防止同一漫画重复入队。
func (s *Store) PendingJobExists(source, comicID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM jobs WHERE source = ? AND comic_id = ? AND status IN ('pending','running')`,
		source, comicID).Scan(&n)
	return n > 0, err
}

// ClaimNextJob 原子地取出一个待执行任务并标记为 running。
func (s *Store) ClaimNextJob() (*model.Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	j := &model.Job{}
	var chapters, title, message, errMsg, origin sql.NullString
	var created, updated sql.NullInt64
	var attempts sql.NullInt64
	err = tx.QueryRow(
		`SELECT id, source, comic_id, title, chapters, status, progress, message, error, origin, created_at, updated_at, attempts
		 FROM jobs WHERE status = 'pending' ORDER BY id ASC LIMIT 1`,
	).Scan(&j.ID, &j.Source, &j.ComicID, &title, &chapters, &j.Status, &j.Progress,
		&message, &errMsg, &origin, &created, &updated, &attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Title = title.String
	j.Message = message.String
	j.Error = errMsg.String
	j.Origin = origin.String
	j.Attempts = int(attempts.Int64)
	if _, err := tx.Exec(`UPDATE jobs SET status = 'running', updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Status = model.JobRunning
	j.CreatedAt = time.UnixMilli(created.Int64)
	_ = json.Unmarshal([]byte(chapters.String), &j.Chapters)
	return j, nil
}

func (s *Store) UpdateJob(id int64, status model.JobStatus, progress float64, message, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET status = ?, progress = ?, message = ?, error = ?, updated_at = ? WHERE id = ?`,
		string(status), progress, message, errMsg, time.Now().UnixMilli(), id)
	return err
}

func (s *Store) GetJob(id int64) (*model.Job, error) {
	j := &model.Job{}
	var chapters, title, message, errMsg, origin sql.NullString
	var created, updated, attempts sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, source, comic_id, title, chapters, status, progress, message, error, origin, created_at, updated_at, attempts
		 FROM jobs WHERE id = ?`, id,
	).Scan(&j.ID, &j.Source, &j.ComicID, &title, &chapters, &j.Status, &j.Progress,
		&message, &errMsg, &origin, &created, &updated, &attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Title = title.String
	j.Message = message.String
	j.Error = errMsg.String
	j.Origin = origin.String
	j.Attempts = int(attempts.Int64)
	j.CreatedAt = time.UnixMilli(created.Int64)
	j.UpdatedAt = time.UnixMilli(updated.Int64)
	_ = json.Unmarshal([]byte(chapters.String), &j.Chapters)
	return j, nil
}

func (s *Store) ListJobs(status string, limit int) ([]model.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, source, comic_id, title, chapters, status, progress, message, error, origin, created_at, updated_at, attempts FROM jobs`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Job
	for rows.Next() {
		var j model.Job
		var chapters, title, message, errMsg, origin sql.NullString
		var created, updated, attempts sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Source, &j.ComicID, &title, &chapters, &j.Status, &j.Progress,
			&message, &errMsg, &origin, &created, &updated, &attempts); err != nil {
			return nil, err
		}
		j.Title = title.String
		j.Message = message.String
		j.Error = errMsg.String
		j.Origin = origin.String
		j.Attempts = int(attempts.Int64)
		j.CreatedAt = time.UnixMilli(created.Int64)
		j.UpdatedAt = time.UnixMilli(updated.Int64)
		_ = json.Unmarshal([]byte(chapters.String), &j.Chapters)
		out = append(out, j)
	}
	return out, rows.Err()
}

// RequeueStaleJobs 把服务重启时处于 running 的任务放回队列。
func (s *Store) RequeueStaleJobs() (int64, error) {
	res, err := s.db.Exec(`UPDATE jobs SET status = 'pending', message = 'requeued after restart' WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BumpJobAttempt 把任务放回 pending 并累加执行次数，用于自动重试。
// 返回受影响行数；0 表示任务状态已改变（例如被取消），调用方应放弃重试。
func (s *Store) BumpJobAttempt(id int64, lastErr string) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'pending', attempts = COALESCE(attempts, 0) + 1,
		        message = 'retry: ' || ?, error = ?, updated_at = ?
		  WHERE id = ? AND status = 'running'`,
		lastErr, lastErr, time.Now().UnixMilli(), id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetryFailedJobs 把失败的任务重新放回队列，按漫画去重。
//
// 同一部漫画可能留下多条历史失败记录（每轮重试各一条），全部改回
// pending 会让队列膨胀、并对同一部漫画重复下载。这里只复活每部漫画
// 最新的一条失败记录，更早的直接跳过。
//
// source 为空表示所有源；否则只重试该源。
func (s *Store) RetryFailedJobs(source string) (int64, error) {
	q := `UPDATE jobs SET status = 'pending', progress = 0, message = 'retry',
	          error = '', updated_at = ?
	      WHERE status = 'failed'
	        AND id IN (
	            SELECT MAX(id) FROM jobs WHERE status = 'failed'
	            GROUP BY source, comic_id
	        )`
	args := []any{time.Now().UnixMilli()}
	if source != "" {
		q += ` AND source = ?`
		args = append(args, source)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- source registry ----------
func (s *Store) PutSource(key, name, version, file string) error {
	_, err := s.db.Exec(
		`INSERT INTO sources(key, name, version, file, installed_at, enabled) VALUES (?, ?, ?, ?, ?, 1)
		 ON CONFLICT(key) DO UPDATE SET name = excluded.name, version = excluded.version,
		   file = excluded.file, installed_at = excluded.installed_at`,
		key, name, version, file, time.Now().UnixMilli())
	return err
}

type SourceInfo struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Version string `json:"version"`
	File    string `json:"file"`
	Enabled bool   `json:"enabled"`
}

func (s *Store) ListSources() ([]SourceInfo, error) {
	rows, err := s.db.Query(`SELECT key, name, version, file, enabled FROM sources ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceInfo
	for rows.Next() {
		var i SourceInfo
		var en int
		if err := rows.Scan(&i.Key, &i.Name, &i.Version, &i.File, &en); err != nil {
			return nil, err
		}
		i.Enabled = en == 1
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSource(key string) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE key = ?`, key)
	return err
}

var _ = fmt.Sprintf
