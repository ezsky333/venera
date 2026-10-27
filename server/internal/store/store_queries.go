package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
)

// UpsertComic 缓存漫画元信息。
func (s *Store) UpsertComic(c model.Comic, sourceType int32) error {
	tags, _ := json.Marshal(c.Tags)
	_, err := s.db.Exec(
		`INSERT INTO comics(source, comic_id, title, subtitle, cover, tags, source_type, updated_at,
		                    author, uploader, description)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source, comic_id) DO UPDATE SET
		   title = COALESCE(NULLIF(excluded.title,''), comics.title),
		   subtitle = COALESCE(NULLIF(excluded.subtitle,''), comics.subtitle),
		   cover = COALESCE(NULLIF(excluded.cover,''), comics.cover),
		   tags = COALESCE(NULLIF(excluded.tags,'[]'), comics.tags),
		   author = COALESCE(NULLIF(excluded.author,''), comics.author),
		   uploader = COALESCE(NULLIF(excluded.uploader,''), comics.uploader),
		   description = COALESCE(NULLIF(excluded.description,''), comics.description),
		   source_type = excluded.source_type,
		   updated_at = excluded.updated_at`,
		c.Source, c.ID, c.Title, c.Subtitle, c.Cover, string(tags), sourceType,
		time.Now().UnixMilli(), c.Author, c.Uploader, c.Description,
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
	var tags, author, uploader, desc sql.NullString
	err := s.db.QueryRow(
		`SELECT source, comic_id, title, subtitle, cover, tags,
		        COALESCE(author,''), COALESCE(uploader,''), COALESCE(description,'')
		 FROM comics WHERE source = ? AND comic_id = ?`,
		source, comicID,
	).Scan(&c.Source, &c.ID, &c.Title, &c.Subtitle, &c.Cover, &tags,
		&author, &uploader, &desc)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tags.Valid && tags.String != "" {
		_ = json.Unmarshal([]byte(tags.String), &c.Tags)
	}
	c.Author = author.String
	c.Uploader = uploader.String
	c.Description = desc.String
	return &c, nil
}

// ---------- favorites ----------

// ErrFavoriteResurrect 表示这条 fav.add 比墓碑更旧，属于"删除前就发出的旧消息"，
// 已拒绝写入。调用方应当据此告知推送方本地也要删掉，否则那台设备会留下幽灵收藏。
var ErrFavoriteResurrect = errors.New("favorite resurrect blocked by tombstone")

// AddFavorite 以 LWW 语义写入收藏。
//
// 两层判定：
//  1. 墓碑判定（跨删除/新增）：若该 (folder, source, comic_id) 在 lamport 更晚
//     的时刻被删除过，这条 add 就是旧消息，直接拒绝并返回 ErrFavoriteResurrect。
//     没有这一步的话，离线队列里迟到的 add 会把用户已经删掉的收藏加回来。
//  2. 行内 LWW（新增/新增）：若已存在且新 lamport 更小，则忽略。
//
// 写入成功时清掉墓碑——用户重新收藏了，墓碑的使命结束。
func (s *Store) AddFavorite(f model.Favorite, lamport int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var tombLamport int64
	var tombExists int
	err = tx.QueryRow(
		`SELECT lamport, 1 FROM favorite_tombstones
		 WHERE folder = ? AND source = ? AND comic_id = ?`,
		f.Folder, f.Source, f.ID,
	).Scan(&tombLamport, &tombExists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if tombExists == 1 && tombLamport >= lamport {
		// 删除发生在这次 add 之后 → 这是迟到的旧消息，拒绝。
		return false, ErrFavoriteResurrect
	}

	tags, _ := json.Marshal(f.Tags)
	res, err := tx.Exec(
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
	if n > 0 {
		// 用户重新收藏了，墓碑的使命结束。
		if _, err := tx.Exec(
			`DELETE FROM favorite_tombstones
			 WHERE folder = ? AND source = ? AND comic_id = ?`,
			f.Folder, f.Source, f.ID,
		); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// RemoveFavorite 删除收藏，并写入墓碑防止旧 add 复活。
//
// lamport 是删除动作发生的时间戳：之后到达的、lamport 更小的 fav.add
// 一律视为"删除前发出的旧消息"被拒绝。没有这个参数时，注释里承诺的
// "防止旧 add 复活"只是一句空话。
func (s *Store) RemoveFavorite(folder, source, comicID string, lamport int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`DELETE FROM favorites WHERE folder = ? AND source = ? AND comic_id = ?`,
		folder, source, comicID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()

	// 即使行不存在也要记墓碑：可能是重复的 remove，也可能是先删后到。
	// 用 MAX 保留更晚的时间戳，避免一条迟到的 remove 把墓碑时间戳改旧。
	if _, err := tx.Exec(
		`INSERT INTO favorite_tombstones(folder, source, comic_id, lamport, deleted_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(folder, source, comic_id) DO UPDATE SET
		   lamport = MAX(excluded.lamport, favorite_tombstones.lamport),
		   deleted_at = excluded.deleted_at`,
		folder, source, comicID, lamport, time.Now().UnixMilli(),
	); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// MoveFavorite 把漫画从一个收藏夹移动到另一个。
//
// 源文件夹的删除同样要记墓碑，否则"移动到别的夹"之后，一条迟到的
// 旧 add 会把它同时塞回源文件夹，表现为移动失败/重复出现。
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
	if _, err := tx.Exec(
		`INSERT INTO favorite_tombstones(folder, source, comic_id, lamport, deleted_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(folder, source, comic_id) DO UPDATE SET
		   lamport = MAX(excluded.lamport, favorite_tombstones.lamport),
		   deleted_at = excluded.deleted_at`,
		fromFolder, f.Source, f.ID, lamport, time.Now().UnixMilli(),
	); err != nil {
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
// HasCachedChapters 判断某漫画在库里是否已有缓存章节。
//
// 用途：入队前过滤"已经下完的书"，避免客户端重复推送收藏时
// 把已完成的漫画重新排队（见 jobs.Enqueue 的说明）。
func (s *Store) HasCachedChapters(source, comicID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM cached_chapters WHERE source = ? AND comic_id = ? LIMIT 1`,
		source, comicID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CachedChapterIDs 返回某漫画已缓存的章节 ID 集合。
//
// 比 ListCachedByComic 轻：只取 chapter_id 一列，供"求源端章节表与
// 已缓存集合的差集"使用（增量下载判断）。
func (s *Store) CachedChapterIDs(source, comicID string) (map[string]bool, error) {
	rows, err := s.db.Query(
		`SELECT chapter_id FROM cached_chapters WHERE source = ? AND comic_id = ?`,
		source, comicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

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
		 FROM jobs WHERE status = 'pending' ORDER BY COALESCE(priority,0) DESC, id ASC LIMIT 1`,
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

// JobProgressHook 是可选的进度观测钩子（测试与运维用，生产可为 nil）。
//
// 在 UpdateJob 落库成功后调用，参数为该任务本次写入的进度值与消息。
// 纯观测用途，不参与业务判定；nil 时零开销。
type JobProgressHook func(id int64, progress float64, message string)

// OnJobProgress 由外部（测试）设置；生产不设置。
var OnJobProgress JobProgressHook

func (s *Store) UpdateJob(id int64, status model.JobStatus, progress float64, message, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET status = ?, progress = ?, message = ?, error = ?, updated_at = ? WHERE id = ?`,
		string(status), progress, message, errMsg, time.Now().UnixMilli(), id)
	if err == nil && OnJobProgress != nil {
		OnJobProgress(id, progress, message)
	}
	return err
}

func (s *Store) GetJob(id int64) (*model.Job, error) {
	j := &model.Job{}
	var chapters, title, message, errMsg, origin sql.NullString
	var created, updated, attempts, priority sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, source, comic_id, title, chapters, status, progress, message, error, origin, created_at, updated_at, attempts, COALESCE(priority,0)
		 FROM jobs WHERE id = ?`, id,
	).Scan(&j.ID, &j.Source, &j.ComicID, &title, &chapters, &j.Status, &j.Progress,
		&message, &errMsg, &origin, &created, &updated, &attempts, &priority)
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
	j.Priority = int(priority.Int64)
	j.CreatedAt = time.UnixMilli(created.Int64)
	j.UpdatedAt = time.UnixMilli(updated.Int64)
	_ = json.Unmarshal([]byte(chapters.String), &j.Chapters)
	return j, nil
}

func (s *Store) ListJobs(status string, limit, offset int) ([]model.Job, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	var args []any
	if status != "" {
		// 兼容面板旧的 "active" 语义：同时取 pending+running
		if status == "active" {
			where = ` WHERE status IN ('pending','running')`
		} else {
			where = ` WHERE status = ?`
			args = append(args, status)
		}
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT id, source, comic_id, title, chapters, status, progress, message, error, origin, created_at, updated_at, attempts, COALESCE(priority,0) FROM jobs` +
		where + ` ORDER BY priority DESC, id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.Job{}
	for rows.Next() {
		var j model.Job
		var chapters, title, message, errMsg, origin sql.NullString
		var created, updated, attempts, priority sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Source, &j.ComicID, &title, &chapters, &j.Status, &j.Progress,
			&message, &errMsg, &origin, &created, &updated, &attempts, &priority); err != nil {
			return nil, 0, err
		}
		j.Title = title.String
		j.Message = message.String
		j.Error = errMsg.String
		j.Origin = origin.String
		j.Attempts = int(attempts.Int64)
		j.Priority = int(priority.Int64)
		j.CreatedAt = time.UnixMilli(created.Int64)
		j.UpdatedAt = time.UnixMilli(updated.Int64)
		_ = json.Unmarshal([]byte(chapters.String), &j.Chapters)
		out = append(out, j)
	}
	return out, total, rows.Err()
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

// ---------- 任务管理操作（面板用） ----------

// PauseJob 暂停任务。pending → paused 立即生效；running 的任务
// 只打上暂停标记，worker 在章节间隙检查标记后把它放回 paused。
func (s *Store) PauseJob(id int64) (string, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'paused', message = 'paused by user', updated_at = ?
		 WHERE id = ? AND status = 'pending'`, now, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return "paused", nil
	}
	// running：标记 message，worker 周期检查（UpdateJob 若已带 paused 标记不覆盖）
	res, err = s.db.Exec(
		`UPDATE jobs SET message = 'pause requested', updated_at = ?
		 WHERE id = ? AND status = 'running'`, now, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return "pause-requested", nil
	}
	return "", nil
}

// ResumeJob 把暂停的任务放回队列。
func (s *Store) ResumeJob(id int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'pending', message = 'resumed', updated_at = ?
		 WHERE id = ? AND status = 'paused'`, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetJobPriority 设置任务优先级（置顶 = 1，取消置顶 = 0）。仅 pending/paused 可调。
func (s *Store) SetJobPriority(id int64, priority int) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET priority = ?, updated_at = ?
		 WHERE id = ? AND status IN ('pending','paused')`,
		priority, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelJob 取消任务。pending/paused 直接置 cancelled；
// running 的情况由调用方（worker 侧）通过状态检查自然终止——
// UpdateJob 在状态已不是 running 时会拒绝写入（见 Worker 端 pause 检查）。
func (s *Store) CancelJob(id int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'cancelled', message = 'cancelled by user', updated_at = ?
		 WHERE id = ? AND status IN ('pending','paused')`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RetryJob 把单个失败/取消的任务重新排队。
func (s *Store) RetryJob(id int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'pending', progress = 0, message = 'retry',
		        error = '', updated_at = ?
		 WHERE id = ? AND status IN ('failed','cancelled')`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// JobIDsByStatus 列出某状态的全部任务 id（批量操作用）。
func (s *Store) JobIDsByStatus(status string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE status = ? ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ErrJobRunning 表示任务正在下载，不能直接删除。
//
// 为什么必须拒绝：worker 只在**章节间隙**通过 GetJob 读 message 判断是否
// 该停下（见 downloader 的 pause 检查）。行被删掉后 GetJob 返回 nil，
// 那个判断直接失效，worker 会继续把整本漫画下完、继续往 library 里写文件、
// 继续插 cached_chapters —— 只是任务记录没了，用户既看不见进度也停不下来。
// 所以"删除运行中的任务"必须先暂停，让 worker 在章节间隙自己退出来。
var ErrJobRunning = errors.New("job is running; pause it first")

// DeleteJob 删除一条任务记录。
//
// 只删任务行，**不动已下载的缓存文件**：缓存是 library 里的实体，
// 有独立的删除入口（按章节/按漫画删），把两者混在一起会让"清理任务列表"
// 变成"删掉用户辛苦下好的内容"，代价太大。
//
// running 的任务返回 ErrJobRunning，理由见该变量注释。
func (s *Store) DeleteJob(id int64) (bool, error) {
	// 先看状态：单条 UPDATE/DELETE 里带 status 条件虽然也能做到，
	// 但那样无法区分"不存在"和"在运行"，前端就没法给出准确提示。
	var status string
	err := s.db.QueryRow(`SELECT status FROM jobs WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status == string(model.JobRunning) {
		return false, ErrJobRunning
	}
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteFinishedJobs 批量删除处于终态的任务（done / failed / cancelled）。
//
// 传 statuses 可只清理其中一部分；空切片表示三种终态全清。
// 返回实际删除的行数。
//
// 只允许终态：pending/paused 是用户还要下载的东西，running 见 ErrJobRunning。
func (s *Store) DeleteFinishedJobs(statuses []string) (int64, error) {
	allowed := map[string]bool{
		string(model.JobDone):      true,
		string(model.JobFailed):    true,
		string(model.JobCancelled): true,
	}
	var picked []string
	if len(statuses) == 0 {
		for k := range allowed {
			picked = append(picked, k)
		}
	} else {
		for _, st := range statuses {
			if !allowed[st] {
				// 静默忽略非法状态，避免一个笔误把 pending 队列清空
				continue
			}
			picked = append(picked, st)
		}
	}
	if len(picked) == 0 {
		return 0, nil
	}

	placeholders := strings.Repeat("?,", len(picked))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(picked))
	for i, st := range picked {
		args[i] = st
	}
	res, err := s.db.Exec(
		`DELETE FROM jobs WHERE status IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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
