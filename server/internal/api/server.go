// Package api 提供 HTTP 接口：同步、库查询、媒体、任务管理。
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/venera-app/venera-nas/internal/jobs"
	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/source"
	"github.com/venera-app/venera-nas/internal/store"
	"github.com/venera-app/venera-nas/internal/syncer"
)

// Server 聚合所有 HTTP 依赖。
type Server struct {
	cfg      Config
	st       *store.Store
	sync     *syncer.Syncer
	src      *source.Manager
	worker   *jobs.Worker
	dl       *jobs.Downloader
	log      *slog.Logger
	started  time.Time
}

// Config 是 API 层需要的配置子集。
type Config struct {
	Token         string
	PublicBaseURL string
	LibraryDir    string
	DataDir       string
	Version       string
	// WebDAV 启用开关
	EnableWebDAV bool
	WebDAVUser   string
	WebDAVPass   string
}

// Deps 是构造 Server 的依赖集合。
type Deps struct {
	Config Config
	Store  *store.Store
	Sync   *syncer.Syncer
	Source *source.Manager
	Worker *jobs.Worker
	DL     *jobs.Downloader
	Logger *slog.Logger
}

// New 创建 API Server。
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Server{
		cfg:     d.Config,
		st:      d.Store,
		sync:    d.Sync,
		src:     d.Source,
		worker:  d.Worker,
		dl:      d.DL,
		log:     d.Logger,
		started: time.Now(),
	}
}

// Handler 返回配置好路由与中间件的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- 公开端点（无需鉴权）----
	mux.HandleFunc("GET /api/v1/ping", s.handlePing)
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)

	// ---- 需要鉴权的 API ----
	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/sync/push", s.handleSyncPush)
	api.HandleFunc("GET /api/v1/sync/pull", s.handleSyncPull)
	api.HandleFunc("GET /api/v1/sync/snapshot", s.handleSyncSnapshot)
	api.HandleFunc("POST /api/v1/sync/source-type", s.handleSourceType)
	api.HandleFunc("GET /api/v1/sync/clients", s.handleClients)

	api.HandleFunc("GET /api/v1/favorites", s.handleFavorites)
	api.HandleFunc("GET /api/v1/folders", s.handleFolders)
	api.HandleFunc("GET /api/v1/history", s.handleHistory)

	api.HandleFunc("GET /api/v1/library", s.handleLibrary)
	api.HandleFunc("GET /api/v1/library/{source}/{comicID}", s.handleLibraryComic)
	api.HandleFunc("DELETE /api/v1/library/{source}/{comicID}/{chapterID}", s.handleDeleteChapter)

	api.HandleFunc("GET /api/v1/chapter", s.handleChapter)
	api.HandleFunc("GET /api/v1/media/{path...}", s.handleMedia)

	api.HandleFunc("POST /api/v1/download", s.handleDownload)
	api.HandleFunc("POST /api/v1/download/favorites", s.handleDownloadFavorites)
	api.HandleFunc("GET /api/v1/jobs", s.handleJobs)
	api.HandleFunc("GET /api/v1/jobs/{id}", s.handleJob)
	api.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.handleCancelJob)

	api.HandleFunc("GET /api/v1/sources", s.handleSources)
	api.HandleFunc("POST /api/v1/sources/refresh", s.handleSourceRefresh)
	api.HandleFunc("POST /api/v1/sources/{key}/probe", s.handleSourceProbe)
	api.HandleFunc("GET /api/v1/sources/{key}/features", s.handleSourceFeatures)

	mux.Handle("/api/v1/", s.authMiddleware(api))

	// ---- WebDAV 兼容（W1 兜底）----
	if s.cfg.EnableWebDAV {
		mux.Handle("/webdav/", s.webdavHandler())
	}

	// ---- 健康检查 ----
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return s.withMiddleware(mux)
}

// withMiddleware 包装日志、恢复、CORS。
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// 简单 CORS，便于浏览器端调试
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Client-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PROPFIND")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": "internal server error",
				})
			}
		}()

		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", rw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// authMiddleware 校验 Bearer Token。
//
// Token 为空时放行（仅建议纯内网场景），并在日志中警告。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := bearerToken(r)
		if got == "" {
			got = r.Header.Get("X-Api-Token")
		}
		// 常量时间比较，避免时序侧信道
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// ---------- 基础端点 ----------

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"service":  "venera-nas",
		"version":  s.cfg.Version,
		"uptime":   time.Since(s.started).Round(time.Second).String(),
		"time":     time.Now().UTC().Format(time.RFC3339),
		"protocol": "w2",
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	libBytes, _ := s.st.LibraryBytes()
	srcs := s.src.List()
	folders, _ := s.st.Folders()
	cursor, _ := s.st.MaxOpID()

	writeJSON(w, http.StatusOK, map[string]any{
		"service":       "venera-nas",
		"version":       s.cfg.Version,
		"protocol":      "w2",
		"authRequired":  s.cfg.Token != "",
		"libraryBytes":  libBytes,
		"sourceCount":   len(srcs),
		"sources":       srcs,
		"folderCount":   len(folders),
		"cursor":        cursor,
		"webdavEnabled": s.cfg.EnableWebDAV,
	})
}

// ---------- 同步 ----------

func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	var req syncer.PushRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	resp, err := s.sync.Push(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	includeSelf := r.URL.Query().Get("includeSelf") == "1"

	resp, err := s.sync.Pull(syncer.PullRequest{
		ClientID:    clientID(r),
		Cursor:      cursor,
		Limit:       limit,
		IncludeSelf: includeSelf,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSyncSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.sync.BuildSnapshot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleSourceType 让客户端上报权威的 sourceKey.hashCode。
// 这是保证长期兼容的关键（不依赖 Dart 内部算法）。
func (s *Server) handleSourceType(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mapping map[string]int32 `json:"mapping"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	n := 0
	for k, v := range req.Mapping {
		if k == "" {
			continue
		}
		if err := s.st.PutSourceType(k, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		n++
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n})
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB().Query(`SELECT client_id, COALESCE(device_name,''), COALESCE(last_seen_at,0), COALESCE(last_op_id,0) FROM clients ORDER BY last_seen_at DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, name string
		var seen, op int64
		if err := rows.Scan(&id, &name, &seen, &op); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"clientId": id, "deviceName": name,
			"lastSeenAt": time.UnixMilli(seen).UTC().Format(time.RFC3339),
			"lastOpId":   op,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": out})
}

// ---------- 收藏 / 历史 ----------

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	items, err := s.st.ListFavorites(folder, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favorites": items, "count": len(items)})
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.st.Folders()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.st.ListHistories(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": items})
}

// ---------- 库查询 ----------

// handleLibrary 列出库中的漫画（按最近更新）。
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB().Query(`
		SELECT source, comic_id, COALESCE(MAX(chapter_title),''), COUNT(DISTINCT chapter_id), SUM(bytes), MAX(created_at)
		FROM cached_chapters
		GROUP BY source, comic_id
		ORDER BY MAX(created_at) DESC LIMIT 200`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()

	type item struct {
		Source    string `json:"source"`
		ComicID   string `json:"comicId"`
		Title     string `json:"title"`
		Chapters  int    `json:"chapters"`
		Bytes     int64  `json:"bytes"`
		UpdatedAt string `json:"updatedAt"`
	}
	var out []item

	// 注意：必须在 rows 关闭之后再补查漫画标题。
	// 若在 Next() 循环内执行嵌套查询，会与同一连接互相等待（SQLite 单写者模型下的死锁）。
	for rows.Next() {
		var it item
		var created int64
		if err := rows.Scan(&it.Source, &it.ComicID, &it.Title, &it.Chapters, &it.Bytes, &created); err != nil {
			continue
		}
		it.UpdatedAt = time.UnixMilli(created).UTC().Format(time.RFC3339)
		out = append(out, it)
	}
	rows.Close()

	for i := range out {
		if c, err := s.st.GetComic(out[i].Source, out[i].ComicID); err == nil && c != nil && c.Title != "" {
			out[i].Title = c.Title
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"library": out})
}

// handleLibraryComic 返回某漫画已缓存的章节列表（客户端据此决定是否走 NAS）。
func (s *Server) handleLibraryComic(w http.ResponseWriter, r *http.Request) {
	srcKey := r.PathValue("source")
	comicID := r.PathValue("comicID")
	if srcKey == "" || comicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicID are required"})
		return
	}
	chapters, err := s.st.ListCachedByComic(srcKey, comicID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 构造客户端可直接使用的相对 URL
	type chapterView struct {
		model.CachedChapter
		Pages []string `json:"pages"`
	}
	out := make([]chapterView, 0, len(chapters))
	base := s.baseURL(r)
	for _, c := range chapters {
		v := chapterView{CachedChapter: c}
		for i := 0; i < c.PageCount; i++ {
			v.Pages = append(v.Pages, fmt.Sprintf("%s/api/v1/media/%s", base,
				urlPathEscape(c.Dir)+fmt.Sprintf("/%d%s", i, s.pageExt(c.Dir, i))))
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":   srcKey,
		"comicId":  comicID,
		"chapters": out,
		"cached":   len(chapters) > 0,
	})
}

func (s *Server) handleDeleteChapter(w http.ResponseWriter, r *http.Request) {
	srcKey := r.PathValue("source")
	comicID := r.PathValue("comicID")
	chapterID := r.PathValue("chapterID")

	ch, err := s.st.GetCachedChapter(srcKey, comicID, chapterID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if ch == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "chapter not cached"})
		return
	}
	dir := filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(ch.Dir))
	if err := os.RemoveAll(dir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.st.DeleteCachedChapter(srcKey, comicID, chapterID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleChapter 是客户端"这一章是否有 NAS 缓存"的核心查询。
//
// 客户端在打开阅读器时调用：
//
//	GET /api/v1/chapter?source=jm&comicId=xxx&chapterId=yyy
//
// cached=true 时返回可直接加载的图片 URL 列表。
func (s *Server) handleChapter(w http.ResponseWriter, r *http.Request) {
	srcKey := r.URL.Query().Get("source")
	comicID := r.URL.Query().Get("comicId")
	chapterID := r.URL.Query().Get("chapterId")
	if srcKey == "" || comicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicId are required"})
		return
	}

	ch, err := s.st.GetCachedChapter(srcKey, comicID, chapterID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if ch == nil {
		writeJSON(w, http.StatusOK, map[string]any{"cached": false})
		return
	}

	base := s.baseURL(r)
	pages := make([]string, 0, ch.PageCount)
	for i := 0; i < ch.PageCount; i++ {
		pages = append(pages, fmt.Sprintf("%s/api/v1/media/%s/%d%s", base,
			urlPathEscape(ch.Dir), i, s.pageExt(ch.Dir, i)))
	}
	var cover string
	if ch.Cover != "" {
		cover = base + "/api/v1/media/" + urlPathEscape(ch.Cover)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cached":  true,
		"pages":   pages,
		"cover":   cover,
		"pages_":  ch.PageCount,
		"bytes":   ch.Bytes,
		"dir":     ch.Dir,
	})
}

// pageExt 探测页面文件的实际扩展名（落盘时按魔数决定了扩展名）。
func (s *Server) pageExt(dir string, index int) string {
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		p := filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(dir), fmt.Sprintf("%d%s", index, ext))
		if _, err := os.Stat(p); err == nil {
			return ext
		}
	}
	return ".jpg"
}

// handleMedia 提供库内文件（图片/封面）。
//
// 安全要点：必须防止路径穿越。这里通过 filepath.Clean + 前缀校验双重保护。
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	rel, err := urlPathUnescape(rel)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	// 归一化并校验
	clean := path.Clean("/" + rel)
	full := filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(clean))
	root, err := filepath.Abs(s.cfg.LibraryDir)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	abs, err := filepath.Abs(full)
	if err != nil || !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// 支持 Range（大图/视频场景）与缓存头
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", mediaType(abs))
	http.ServeFile(w, r, abs)
}

func mediaType(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".cbz":
		return "application/vnd.comicbook+zip"
	case ".epub":
		return "application/epub+zip"
	}
	return "application/octet-stream"
}

// ---------- 下载 / 任务 ----------

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source   string   `json:"source"`
		ComicID  string   `json:"comicId"`
		Title    string   `json:"title,omitempty"`
		Chapters []string `json:"chapters,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if req.Source == "" || req.ComicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and comicId are required"})
		return
	}
	if !s.src.Has(req.Source) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown source: " + req.Source})
		return
	}

	id, err := s.worker.Enqueue(req.Source, req.ComicID, req.Title, "manual", req.Chapters)
	if errors.Is(err, jobs.ErrAlreadyQueued) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already queued"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"jobId": id, "status": "pending"})
}

func (s *Server) handleDownloadFavorites(w http.ResponseWriter, r *http.Request) {
	n, err := s.worker.QueueFavorites()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": n})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.st.ListJobs(status, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": list})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	job, err := s.st.GetJob(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if err := s.st.UpdateJob(id, model.JobCancelled, 0, "cancelled by user", ""); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// ---------- 源管理 ----------

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sources": s.src.List()})
}

func (s *Server) handleSourceRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.src.Scan(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": s.src.List()})
}

func (s *Server) handleSourceProbe(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		ComicID string `json:"comicId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ComicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "comicId is required"})
		return
	}
	detail, err := s.src.LoadDetail(key, req.ComicID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "title": detail.Title,
		"chapters": len(detail.Chapters), "grouped": detail.Grouped,
	})
}

func (s *Server) handleSourceFeatures(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	features := []string{}
	for _, f := range []string{"favorites.loadComics", "account.login", "comic.loadComments"} {
		if has, err := s.src.HasFeature(key, f); err == nil && has {
			features = append(features, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": key, "features": features})
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func clientID(r *http.Request) string {
	if v := r.URL.Query().Get("clientId"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Client-Id"); v != "" {
		return v
	}
	return ""
}

// baseURL 决定返回给客户端的绝对 URL 前缀。
// 优先使用配置（反代场景），否则按请求推断。
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicBaseURL != "" {
		return s.cfg.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// urlPathEscape 逐段转义路径（保留 /）。
func urlPathEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = pathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func urlPathUnescape(p string) (string, error) {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		v, err := pathUnescape(seg)
		if err != nil {
			return "", err
		}
		parts[i] = v
	}
	return strings.Join(parts, "/"), nil
}
