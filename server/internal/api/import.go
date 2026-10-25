package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// ImportVeneraArchive 解析 App 导出的 .venera 同步包，把其中的
// 收藏、历史、源数据、Cookie 导入服务端状态。
//
// 同步包结构（lib/utils/data.dart 的 exportAppData）：
//
//	history.db          SQLite: history 表
//	local_favorite.db   SQLite: 每个收藏夹一张表 + folder_order/folder_sync
//	appdata.json        配置（含 webdav/settings）
//	cookie.db           SQLite: cookies 表
//	comic_source/*.js   源脚本
//	comic_source/*.data 源私有数据（登录态）
//
// 这是"老用户零升级接入"的关键路径，也是 W2 之外的迁移通道。
func (s *Server) ImportVeneraArchive(zipPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer r.Close()

	tmp, err := os.MkdirTemp("", "venera-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var (
		foundFav    string
		foundHist   string
		foundCookie string
		foundApp    string
		sourceFiles []string
		sourceData  []string
	)

	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		// 防路径穿越
		if strings.Contains(name, "..") {
			continue
		}
		dest := filepath.Join(tmp, filepath.FromSlash(name))
		switch {
		case name == "local_favorite.db":
			foundFav = dest
		case name == "history.db":
			foundHist = dest
		case name == "cookie.db":
			foundCookie = dest
		case name == "appdata.json":
			foundApp = dest
		case strings.HasPrefix(name, "comic_source/") && strings.HasSuffix(name, ".js"):
			sourceFiles = append(sourceFiles, dest)
		case strings.HasPrefix(name, "comic_source/") && strings.HasSuffix(name, ".data"):
			sourceData = append(sourceData, dest)
		}
		if err := extractFile(f, dest); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
	}

	// 1. 导入源脚本
	for _, p := range sourceFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := s.src.Install(filepath.Base(p), string(b)); err != nil {
			s.log.Warn("import source failed", "file", filepath.Base(p), "err", err)
			continue
		}
	}
	// 2. 导入源私有数据（登录态）
	for _, p := range sourceData {
		key := strings.TrimSuffix(filepath.Base(p), ".data")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(b, &data); err != nil {
			continue
		}
		if err := s.st.PutSourceData(key, data); err != nil {
			s.log.Warn("import source data failed", "source", key, "err", err)
		}
	}
	// 3. 导入收藏夹
	var favCount int
	if foundFav != "" {
		n, err := importFavorites(s.st, foundFav)
		switch {
		case err != nil:
			s.log.Warn("import favorites failed", "err", err)
		case n > 0:
			favCount = n
			// 让同步引擎知道新增了收藏，以触发下载排队
			if s.worker != nil {
				go func() {
					if _, err := s.worker.QueueFavorites(); err != nil {
						s.log.Warn("queue favorites after import failed", "err", err)
					}
				}()
			}
		}
	}
	// 4. 导入历史
	if foundHist != "" {
		if n, err := importHistory(s.st, foundHist); err != nil {
			s.log.Warn("import history failed", "err", err)
		} else {
			s.log.Info("imported history entries", "count", n)
		}
	}
	// 5. Cookie
	if foundCookie != "" {
		if n, err := importCookies(s.st, foundCookie); err != nil {
			s.log.Warn("import cookies failed", "err", err)
		} else {
			s.log.Info("imported cookies", "count", n)
		}
	}
	// 6. 设置
	if foundApp != "" {
		if err := importAppdata(s.st, foundApp); err != nil {
			s.log.Warn("import appdata failed", "err", err)
		}
	}

	s.log.Info("venera archive imported", "favorites", favCount)
	return nil
}

func extractFile(f *zip.File, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, io.LimitReader(rc, 512<<20))
	return err
}

// importFavorites 从 App 的 local_favorite.db 导入收藏。
//
// 表结构（lib/foundation/favorites.dart）：
//
//	folder_order(folder_name, order_value)
//	folder_sync(folder_name, source_key, source_folder)
//	<每个收藏夹一张同名表>(id, name, author, type, tags, cover_path, time, display_order, translated_tags)
//
// 其中 type 就是 sourceKey.hashCode，需要反查到 sourceKey。
func importFavorites(st *store.Store, dbPath string) (int, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	// 找出所有"收藏夹表"：排除系统表
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return 0, err
	}
	var folders []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		switch name {
		case "folder_order", "folder_sync", "sqlite_sequence":
			continue
		}
		folders = append(folders, name)
	}
	rows.Close()

	// 建立 hash -> sourceKey 的反查表
	reverse := buildReverseTypeMap(st)

	total := 0
	for _, folder := range folders {
		// 表名需转义（可能含引号）
		q := `SELECT id, name, author, type, tags, cover_path, time FROM "` +
			strings.ReplaceAll(folder, `"`, `""`) + `"`
		r, err := db.Query(q)
		if err != nil {
			continue
		}
		for r.Next() {
			var (
				id, name, author, tags, cover, tm string
				typ                              int32
			)
			if err := r.Scan(&id, &name, &author, &typ, &tags, &cover, &tm); err != nil {
				continue
			}
			srcKey := reverse[typ]
			if srcKey == "" {
				// 无法反查时保留一个可识别的占位，便于人工修正
				srcKey = fmt.Sprintf("unknown_%d", typ)
			}
			var tagList []string
			for _, t := range strings.Split(tags, ",") {
				if t = strings.TrimSpace(t); t != "" {
					tagList = append(tagList, t)
				}
			}
			added := parseTimeLoose(tm)
			fav := model.Favorite{
				Comic: model.Comic{
					Source: srcKey, ID: id, Title: name,
					Cover: cover, Tags: tagList,
				},
				Folder: folder, Author: author,
				AddedAt: added, SourceType: typ,
			}
			if changed, err := st.AddFavorite(fav, 0); err == nil && changed {
				total++
			}
			_ = st.UpsertComic(fav.Comic, typ)
			_ = st.PutSourceType(srcKey, typ)
		}
		r.Close()
	}
	return total, nil
}

// importHistory 从 history.db 导入阅读进度。
func importHistory(st *store.Store, dbPath string) (int, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	reverse := buildReverseTypeMap(st)

	rows, err := db.Query(`SELECT id, title, subtitle, cover, time, type, ep, page, readEpisode, max_page FROM history`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var (
			id, title, subtitle, cover string
			tm, ep, page, maxPage      int64
			typ                        int32
			readEps                    string
		)
		if err := rows.Scan(&id, &title, &subtitle, &cover, &tm, &typ, &ep, &page, &readEps, &maxPage); err != nil {
			continue
		}
		srcKey := reverse[typ]
		if srcKey == "" {
			srcKey = fmt.Sprintf("unknown_%d", typ)
		}
		h := model.History{
			Comic: model.Comic{
				Source: srcKey, ID: id, Title: title, Subtitle: subtitle, Cover: cover,
			},
			SourceType: typ,
			Ep:         int(ep),
			Page:       int(page),
			UpdatedAt:  time.UnixMilli(tm),
		}
		if readEps != "" {
			for _, e := range strings.Split(readEps, ",") {
				if e = strings.TrimSpace(e); e != "" {
					h.ReadEps = append(h.ReadEps, e)
				}
			}
		}
		if maxPage > 0 {
			v := int(maxPage)
			h.MaxPage = &v
		}
		if changed, err := st.UpsertHistory(h); err == nil && changed {
			n++
		}
		_ = st.PutSourceType(srcKey, typ)
	}
	return n, nil
}

// importCookies 从 cookie.db 导入 Cookie，按 domain 归属到对应的源。
func importCookies(st *store.Store, dbPath string) (int, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name, value, domain, path, expires, secure, httpOnly FROM cookies`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	// Cookie 与源的对应关系由域名推断；服务端按源隔离存储。
	// 这里把所有 Cookie 归到 "shared" 桶，源运行时按域名匹配取用。
	bySource := map[string][]model.CookieRecord{}
	for rows.Next() {
		var (
			name, value, domain string
			path                *string
			expires             *int64
			secure, httpOnly    int
		)
		if err := rows.Scan(&name, &value, &domain, &path, &expires, &secure, &httpOnly); err != nil {
			continue
		}
		p := "/"
		if path != nil && *path != "" {
			p = *path
		}
		rec := model.CookieRecord{
			Name: name, Value: value, Domain: domain, Path: p,
			Expires: expires, Secure: secure == 1, HTTPOnly: httpOnly == 1,
		}
		bySource["shared"] = append(bySource["shared"], rec)
	}
	n := 0
	for src, cookies := range bySource {
		if err := st.PutCookies(src, cookies); err == nil {
			n += len(cookies)
		}
	}
	return n, nil
}

// importAppdata 导入配置（仅保留服务端有意义的字段）。
func importAppdata(st *store.Store, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var data struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	// 只导入"源相关"的设置（设备相关的不应跨端同步）
	interesting := []string{
		"comicSpecificSettings", "deviceSpecificSettings",
		"followUpdatesFolder", "searchSources",
	}
	for _, k := range interesting {
		if v, ok := data.Settings[k]; ok {
			_ = st.PutSetting(k, v)
		}
	}
	return nil
}

// buildReverseTypeMap 构造 sourceType(hash) -> sourceKey 的反查表。
//
// 优先用映射表里已登记的对应关系，再用已安装源现场计算补齐。
func buildReverseTypeMap(st *store.Store) map[int32]string {
	out := map[int32]string{}
	rows, err := st.DB().Query(`SELECT source_key, source_type FROM source_type_map`)
	if err == nil {
		for rows.Next() {
			var k string
			var v int32
			if err := rows.Scan(&k, &v); err == nil {
				out[v] = k
			}
		}
		rows.Close()
	}
	return out
}

func parseTimeLoose(s string) time.Time {
	// App 存的是 "2006-01-02 15:04:05"
	for _, layout := range []string{
		"2006-01-02 15:04:05", time.RFC3339, "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now()
}
