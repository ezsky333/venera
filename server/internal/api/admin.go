package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// 本文件实现 /api/v1/admin/* 与内嵌的 /admin 面板。
//
// 设计取舍：
//   - 面板是**单文件内嵌 HTML**，不引入构建链。NAS 上的 Docker 镜像要尽量小、
//     部署要尽量无脑，为此多一个前端工程不划算。
//   - 面板自己用 fetch 调同一套 API（Bearer token 由页面上的输入框提供，
//     存在 sessionStorage），因此不需要单独的会话机制。
//   - 所有写操作都要求 Bearer token；读操作与其它 API 一样受 authMiddleware 保护。

// handleAdminStats 返回面板顶部总览。
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleAdminLibrary 返回库列表（分页 + 按源过滤）。
func (s *Server) handleAdminLibrary(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("source")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	items, total, err := s.st.ListLibrary(src, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 补上可直接访问的封面 URL。
	// 封面扩展名由落盘时的魔数决定（png/jpg/webp…），不能假设 .png，
	// 否则页面上会整排空白 —— 文件其实都在。
	base := s.baseURL(r)
	tq := s.mediaTokenQuery()
	for i := range items {
		if items[i].Cover == "" {
			continue
		}
		if p := s.probeCoverPath(items[i].Cover); p != "" {
			items[i].Cover = base + "/api/v1/media/" + urlPathEscape(p) + tq
		} else {
			items[i].Cover = ""
		}
	}
	// items 为空时给空数组而不是 null：面板的 data.items.map 遇到 null
	// 会抛 "Cannot read properties of null (reading 'map')"，切页直接报错。
	if items == nil {
		items = []store.LibraryItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

// probeCoverPath 探测漫画目录下封面的真实文件名。
//
// 返回相对路径（如 "标题 [id]/cover.jpg"），找不到时返回空串。
func (s *Server) probeCoverPath(comicDir string) string {
	for _, ext := range store.CoverExtCandidates {
		rel := comicDir + "/cover" + ext
		full := filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(rel))
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return rel
		}
	}
	return ""
}

// handleAdminChapters 返回某漫画的缓存章节。
func (s *Server) handleAdminChapters(w http.ResponseWriter, r *http.Request) {
	srcKey := r.URL.Query().Get("source")
	comicID := r.URL.Query().Get("comicId")
	if srcKey == "" || comicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicId are required"})
		return
	}
	chapters, err := s.st.ChaptersOf(srcKey, comicID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chapters": chapters})
}

// handleAdminFolders 返回收藏夹概况。
func (s *Server) handleAdminFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.st.FolderStats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

// handleAdminDeleteComic 删除一本漫画的全部缓存。
func (s *Server) handleAdminDeleteComic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source  string `json:"source"`
		ComicID string `json:"comicId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Source == "" || req.ComicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicId are required"})
		return
	}
	dirs, err := s.st.DeleteComicCache(req.Source, req.ComicID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 删文件：先删各章目录，再尝试删空的漫画目录
	removed := 0
	for _, d := range dirs {
		full := filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(d))
		if err := os.RemoveAll(full); err == nil {
			removed++
		}
	}
	s.log.Info("admin delete comic", "source", req.Source, "comic", req.ComicID, "dirs", removed)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": removed})
}

// handleAdminDeleteChapter 删除单章缓存。
func (s *Server) handleAdminDeleteChapter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source    string `json:"source"`
		ComicID   string `json:"comicId"`
		ChapterID string `json:"chapterId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Source == "" || req.ComicID == "" || req.ChapterID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source, comicId and chapterId are required"})
		return
	}

	ch, err := s.st.GetCachedChapter(req.Source, req.ComicID, req.ChapterID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if ch == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "chapter not cached"})
		return
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(ch.Dir))); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.st.DeleteCachedChapter(req.Source, req.ComicID, req.ChapterID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleAdminJobs 返回任务列表（面板的下载进度视图）。
//
// 支持分页与状态筛选；jobs 表为空时也返回空数组而非 null，
// 面板的 list.map 才不会抛 "Cannot read properties of null"。
func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	list, total, err := s.st.ListJobs(status, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []model.Job{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": list, "items": list,
		"total": total, "limit": limit, "offset": offset,
	})
}

// handleAdminComic 返回漫画的缓存详情 + 元信息（面板预览用）。
func (s *Server) handleAdminComic(w http.ResponseWriter, r *http.Request) {
	srcKey := r.URL.Query().Get("source")
	comicID := r.URL.Query().Get("comicId")
	if srcKey == "" || comicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicId are required"})
		return
	}

	out := map[string]any{"source": srcKey, "comicId": comicID}

	// 已缓存的元信息（即使离线也能给出标题/标签）
	if c, err := s.st.GetComic(srcKey, comicID); err == nil && c != nil {
		out["title"] = c.Title
		out["subtitle"] = c.Subtitle
		out["tags"] = c.Tags
	}

	// 库内章节（用它反推漫画目录，再探测封面真实文件名）
	chapters, err := s.st.ChaptersOf(srcKey, comicID)
	if err == nil {
		out["chapters"] = chapters
		if len(chapters) > 0 {
			if dir, ok := chapters[0]["dir"].(string); ok && dir != "" {
				comicDir := dir
				if i := strings.IndexByte(comicDir, '/'); i > 0 {
					comicDir = comicDir[:i]
				}
				base := s.baseURL(r)
				tq := s.mediaTokenQuery()
				if p := s.probeCoverPath(comicDir); p != "" {
					out["cover"] = base + "/api/v1/media/" + urlPathEscape(p) + tq
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleComicFallback 是详情页的**离线兜底**接口。
//
// 场景：客户端打开详情页时源站点不可达（超时/被墙/临时故障），
// 原版行为是一直转圈或者报错。但只要 NAS 上存过这本漫画的元信息，
// 就足以渲染一个可用的详情页：标题、封面、标签、以及已缓存的章节。
//
// 与 /api/v1/admin/comic 的区别：
//   - 这个是给普通客户端用的，URL 里的封面/页面可直接加载；
//   - 章节按客户端期望的结构返回（id + title），便于直接喂给阅读器。
func (s *Server) handleComicFallback(w http.ResponseWriter, r *http.Request) {
	srcKey := r.URL.Query().Get("source")
	comicID := r.URL.Query().Get("comicId")
	if srcKey == "" || comicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicId are required"})
		return
	}

	base := s.baseURL(r)
	tq := s.mediaTokenQuery()

	out := map[string]any{
		"source": srcKey,
		"comicId": comicID,
		"cached":  false,
	}

	// 元信息
	var title, subtitle, cover, tags string
	if c, err := s.st.GetComic(srcKey, comicID); err == nil && c != nil {
		title, subtitle, cover = c.Title, c.Subtitle, c.Cover
		if len(c.Tags) > 0 {
			if b, err := json.Marshal(c.Tags); err == nil {
				tags = string(b)
			}
		}
		out["title"] = title
		out["subtitle"] = subtitle
		out["tags"] = c.Tags
	}

	// 已缓存章节
	chapters, err := s.st.ChaptersOf(srcKey, comicID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(chapters) > 0 {
		out["cached"] = true

		// 已缓存章节的明细（面板与阅读器共用）
		cachedByID := make(map[string]map[string]any, len(chapters))
		list := make([]map[string]any, 0, len(chapters))
		for _, ch := range chapters {
			item := map[string]any{
				"id":    ch["chapterId"],
				"title": ch["title"],
				"pages": ch["pages"],
			}
			list = append(list, item)
			if id, ok := ch["chapterId"].(string); ok {
				cachedByID[id] = item
			}
		}
		out["chapters"] = list

		// ---- 完整章节表（关键）----
		//
		// 客户端阅读器用 history.ep 当作**下标**去章节数组里取章节，
		// 所以返回的数组必须与源端顺序一一对应。只回已缓存的章节会让
		// 下标整体前移：history 里第 19 章，在压缩表里成了第 19 个
		// 已缓存章节，打开的就是完全不相干的一话（实测 E39 vs 19）。
		//
		// 因此这里优先返回源端全量列表，并把每章是否已缓存显式标出；
		// 客户端据此渲染灰掉的未缓存章节，下标关系永远成立。
		if full, err := s.st.GetComicChapterList(srcKey, comicID); err == nil && len(full) > 0 {
			fullList := make([]map[string]any, 0, len(full))
			hasCached := make([]bool, 0, len(full))
			for _, c := range full {
				_, ok := cachedByID[c.ID]
				fullList = append(fullList, map[string]any{
					"id":      c.ID,
					"title":   c.Title,
					"group":   c.Group,
					"cached":  ok,
				})
				hasCached = append(hasCached, ok)
			}
			out["fullChapters"] = fullList
			out["cachedFlags"] = hasCached
		}

		// 封面：优先用磁盘上的实际文件
		if dir, ok := chapters[0]["dir"].(string); ok && dir != "" {
			comicDir := dir
			if i := strings.IndexByte(comicDir, '/'); i > 0 {
				comicDir = comicDir[:i]
			}
			if p := s.probeCoverPath(comicDir); p != "" {
				out["cover"] = base + "/api/v1/media/" + urlPathEscape(p) + tq
			}
		}
	}

	// 没有本地封面时回落到源站的封面 URL：
	// 客户端加载失败会自动用占位图，比"整个详情页加载失败"好得多。
	if _, ok := out["cover"]; !ok && cover != "" {
		out["cover"] = cover
	}
	_ = tags

	writeJSON(w, http.StatusOK, out)
}

// handleAdminIndex 返回内嵌面板。
//
// 注意：面板本身是静态资源，但其中调用的全是需要 token 的 API。
// 因此这里不做额外鉴权（页面是空壳，不含数据），token 由用户填写。
func (s *Server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(adminHTML))
}
