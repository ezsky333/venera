// Package api 提供 HTTP 接口：同步、库查询、媒体、任务管理。
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	cfg    Config
	st     *store.Store
	sync   *syncer.Syncer
	src    *source.Manager
	worker *jobs.Worker
	dl     *jobs.Downloader
	// hub 是实时进度中心（可为 nil：退化为只读数据库）。
	hub     *jobs.Hub
	log     *slog.Logger
	started time.Time
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
	Hub    *jobs.Hub
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
		hub:     d.Hub,
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
	api.HandleFunc("GET /api/v1/comic", s.handleComicFallback)
	api.HandleFunc("GET /api/v1/media/{path...}", s.handleMedia)

	api.HandleFunc("POST /api/v1/download", s.handleDownload)
	api.HandleFunc("POST /api/v1/download/favorites", s.handleDownloadFavorites)
	// 同一个 handler 支持 GET 是给"补录预览"用的：dryRun 是只读操作，
	// 用 GET 语义更准，也方便直接粘到浏览器地址栏看结果。
	api.HandleFunc("GET /api/v1/download/favorites", s.handleDownloadFavorites)
	api.HandleFunc("POST /api/v1/jobs/queue-favorites", s.handleDownloadFavorites)
	api.HandleFunc("GET /api/v1/jobs/queue-favorites", s.handleDownloadFavorites)
	// 补抓存量封面：只下载缺失的 cover，不动已有内页
	api.HandleFunc("POST /api/v1/download/covers", s.handleRefetchCovers)
	api.HandleFunc("GET /api/v1/jobs", s.handleJobs)
	api.HandleFunc("GET /api/v1/jobs/events", s.handleJobsEvents)
	api.HandleFunc("GET /api/v1/jobs/by-comic", s.handleComicJobs)
	api.HandleFunc("POST /api/v1/jobs/bulk", s.handleJobsBulk)
	api.HandleFunc("GET /api/v1/jobs/{id}", s.handleJob)
	api.HandleFunc("POST /api/v1/jobs/{id}/{action}", s.handleJobAction)
	api.HandleFunc("POST /api/v1/jobs/retry", s.handleRetryFailed)

	api.HandleFunc("GET /api/v1/sources", s.handleSources)
	api.HandleFunc("GET /api/v1/sources/bundle", s.handleSourceBundles)
	api.HandleFunc("GET /api/v1/sources/{key}/script", s.handleSourceScript)
	api.HandleFunc("POST /api/v1/sources/refresh", s.handleSourceRefresh)
	api.HandleFunc("POST /api/v1/sources/{key}/probe", s.handleSourceProbe)
	api.HandleFunc("GET /api/v1/sources/{key}/features", s.handleSourceFeatures)

	// ---- 管理面板 ----
	api.HandleFunc("GET /api/v1/admin/stats", s.handleAdminStats)
	api.HandleFunc("GET /api/v1/admin/library", s.handleAdminLibrary)
	api.HandleFunc("GET /api/v1/admin/chapters", s.handleAdminChapters)
	api.HandleFunc("GET /api/v1/admin/comic", s.handleAdminComic)
	api.HandleFunc("GET /api/v1/admin/folders", s.handleAdminFolders)
	api.HandleFunc("GET /api/v1/admin/jobs", s.handleAdminJobs)
	api.HandleFunc("POST /api/v1/admin/comic/delete", s.handleAdminDeleteComic)
	api.HandleFunc("POST /api/v1/admin/chapter/delete", s.handleAdminDeleteChapter)

	mux.Handle("/api/v1/", s.authMiddleware(api))

	// ---- 管理面板页面（静态空壳，数据靠 token 拉取）----
	mux.HandleFunc("GET /admin", s.handleAdminIndex)
	mux.HandleFunc("GET /admin/", s.handleAdminIndex)

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

// Flush 透传底层 Flusher：SSE（/api/v1/jobs/events）依赖它做流式推送。
// ResponseWriter 被包装后接口集会缩小，不显式实现 Flush 的话
// handler 侧的类型断言会失败，SSE 直接退化为 500。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
		// 图片/封面这类请求由 <img>/播放器直接发起，无法附加自定义 header，
		// 所以接受 ?token= 查询参数。仅对只读的 media 路径放宽，
		// 避免把 token 泄漏进会写数据的接口 URL 里。
		// SSE（jobs/events）同理：EventSource 是浏览器原生构造，
		// 无法带 Authorization header，只能走查询参数。
		if got == "" &&
			(strings.HasPrefix(r.URL.Path, "/api/v1/media/") ||
				r.URL.Path == "/api/v1/jobs/events") {
			got = r.URL.Query().Get("token")
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
	tq := s.mediaTokenQuery()
	for _, c := range chapters {
		v := chapterView{CachedChapter: c}
		for i := 0; i < c.PageCount; i++ {
			v.Pages = append(v.Pages, fmt.Sprintf("%s/api/v1/media/%s%s", base,
				urlPathEscape(c.Dir)+fmt.Sprintf("/%d%s", i, s.pageExt(c.Dir, i)), tq))
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
	tq := s.mediaTokenQuery()
	pages := make([]string, 0, ch.PageCount)
	for i := 0; i < ch.PageCount; i++ {
		pages = append(pages, fmt.Sprintf("%s/api/v1/media/%s/%d%s%s", base,
			urlPathEscape(ch.Dir), i, s.pageExt(ch.Dir, i), tq))
	}
	var cover string
	if ch.Cover != "" {
		cover = base + "/api/v1/media/" + urlPathEscape(ch.Cover) + tq
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cached": true,
		"pages":  pages,
		"cover":  cover,
		"pages_": ch.PageCount,
		"bytes":  ch.Bytes,
		"dir":    ch.Dir,
	})
}

// mediaTokenQuery 返回附加在 media URL 上的鉴权查询串。
//
// 图片由 <img>/播放器直接拉取，加不了 Authorization header，
// 只能把 token 放进 URL。未配置 token 时返回空串。
func (s *Server) mediaTokenQuery() string {
	if s.cfg.Token == "" {
		return ""
	}
	return "?token=" + url.QueryEscape(s.cfg.Token)
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

// handleDownload 请求下载一部漫画（或指定章节）。
//
// 关键语义：**增量**。
//
// 旧实现调 worker.Enqueue，只要库里有任意一章缓存就返回 409
// "already cached"，于是"已缓存 10 章、源站更新到 12 章"的用户
// 无论点多少次"推送到 NAS"都下不到后 2 章——看起来像成功了，
// 其实什么都没发生。
//
// 现在走 EnqueueIncremental：拿源端章节表(chapter_list)与已缓存集合
// 求差集，只把缺失的章节写进任务。响应里带上 total/cached/missing，
// 客户端据此能明确告诉用户"只补了 2 章"还是"已经是最新"。
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

	res, err := s.worker.EnqueueIncremental(req.Source, req.ComicID, req.Title, "manual", req.Chapters)
	if errors.Is(err, jobs.ErrAlreadyQueued) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "already queued", "reason": "already-queued",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !res.Queued {
		// 已是最新：不是错误。200 + queued:false 让客户端可以区分
		// "真的排上了任务" 和 "确实不需要下"。
		writeJSON(w, http.StatusOK, map[string]any{
			"queued": false, "reason": res.Reason,
			"total": res.Total, "cached": res.Cached,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"jobId": res.JobID, "status": "pending", "queued": true,
		"total": res.Total, "cached": res.Cached,
		"missing": len(res.Missing), "chapters": res.Missing,
	})
}

// handleDownloadFavorites 补录"收藏了但没缓存"的漫画到下载队列。
//
// GET/POST /api/v1/jobs/queue-favorites?dryRun=1
//
// dryRun=1 只返回计划（会入队多少本、跳过多少、按源分布、前 50 条明细），
// 不写库；不带则实际入队。面板先预览再确认，避免用户盲点一个按钮
// 就把几百本塞进队列。
//
// 判定与收藏自动同步(QueueFavorites)共用同一段逻辑，区别只有 origin：
// 这里写 favorite-backfill，自动同步写 favorite-sync。
func (s *Server) handleDownloadFavorites(w http.ResponseWriter, r *http.Request) {
	if s.worker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "worker not started",
		})
		return
	}
	dryRun := r.URL.Query().Get("dryRun")
	isDry := dryRun == "1" || dryRun == "true"

	plan, err := s.worker.QueueFavoritesDetailed(isDry, "favorite-backfill")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !isDry {
		s.log.Info("favorites backfill", "queued", plan.Queued,
			"candidates", plan.Candidates, "failed", plan.Failed)
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleRefetchCovers 补抓"已缓存但缺封面"的漫画的封面。
//
// 全量下载任务不重跑（重跑 = 重新下载所有内页），所以早期因封面
// 301 失败留下的缺口需要这个独立入口来补。
func (s *Server) handleRefetchCovers(w http.ResponseWriter, r *http.Request) {
	tried, ok, err := s.dl.RefetchCovers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tried": tried, "fetched": ok,
	})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	list, total, err := s.st.ListJobs(status, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if list == nil {
		list = []model.Job{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": list, "total": total})
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
	ok, err := s.st.CancelJob(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		job, _ := s.st.GetJob(id)
		st := ""
		if job != nil {
			st = string(job.Status)
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "cannot cancel job in status " + st})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// handleRetryFailed 把失败的任务重新排队（按漫画去重）。
//
// 典型场景：改了源配置、换了代理、或修了服务端 bug 之后，
// 之前失败的几百个收藏不该被永久放弃，需要一次批量重试。
func (s *Server) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	n, err := s.st.RetryFailedJobs(req.Source)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.log.Info("retry", "requeued", n, "source", req.Source)
	writeJSON(w, http.StatusOK, map[string]any{"requeued": n})
}

// ---------- 源管理 ----------
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sources": s.src.List()})
}

// handleSourceBundles 一次性下发所有源的脚本。
//
// 客户端首次连接 NAS 时调用，把服务端已有的源装到本地，
// 这样"新设备装好就有源"，不用让用户手动导入 34 个 JS 文件。
func (s *Server) handleSourceBundles(w http.ResponseWriter, r *http.Request) {
	bundles := s.src.Bundles()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(bundles),
		"sources": bundles,
	})
}

// handleSourceScript 下发单个源的脚本（用于增量补齐/更新）。
func (s *Server) handleSourceScript(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	b, ok := s.src.Bundle(key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "source not found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Source-Version", b.Version)
	_, _ = w.Write([]byte(b.Script))
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
