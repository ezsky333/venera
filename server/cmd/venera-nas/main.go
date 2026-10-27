// Command venera-nas 是 Venera 漫画 App 的 NAS 服务端。
//
// 它承担三件事：
//  1. 跑漫画源（用 goja 复现 App 的 JS 运行环境），把收藏夹内容下载到 NAS
//  2. 提供 W2 增量同步协议（操作日志 + 游标拉取），替代原来整包覆盖的 WebDAV
//  3. 当客户端阅读时提供"已缓存章节"的直出图片，实现 NAS 优先读取
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/jshost"
	"github.com/venera-app/venera-nas/internal/api"
	"github.com/venera-app/venera-nas/internal/config"
	"github.com/venera-app/venera-nas/internal/jobs"
	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/source"
	"github.com/venera-app/venera-nas/internal/store"
	"github.com/venera-app/venera-nas/internal/syncer"
)

// version 由构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		doMigrate   = flag.Bool("import", false, "import a .venera archive then exit")
		importPath  = flag.String("archive", "", "path to .venera archive (with -import)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	if err := run(cfg, logger, *doMigrate, *importPath); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger, doImport bool, archive string) error {
	// ---- 存储 ----
	st, err := store.Open(cfg.DataDir, cfg.LibraryDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}

	// ---- 源管理器 ----
	srcMgr, err := source.NewManager(source.Options{
		DataDir:  cfg.DataDir,
		Store:    &storeAdapter{st},
		InitJS:   assets.InitJS,
		Logger:   func(level, title, content string) {
			if level == "error" {
				logger.Error("source", "title", title, "msg", content)
			} else if level == "warning" {
				logger.Warn("source", "title", title, "msg", content)
			}
		},
		RPS:      cfg.GlobalRPS,
		Timeout:  150 * time.Second,
		Platform: "linux",
		Locale:   "zh_CN",
		Proxy:    jshost.ProxyConfig{Raw: cfg.Proxy, Bypass: cfg.ProxyBypass},
	})
	if err != nil {
		return fmt.Errorf("source manager: %w", err)
	}
	logger.Info("sources loaded", "count", len(srcMgr.List()))

	// ---- 同步引擎 ----
	syncEngine := syncer.New(st)

	// ---- 实时进度中心 ----
	//
	// 进度是高频变化数据（每张图都在变），写库太贵、读库太慢。
	// Hub 把进度留在内存、由 SSE 每 200ms 合并推给面板；
	// SQLite 只做 5 秒检查点。见 internal/jobs/live.go。
	hub := jobs.NewHub()
	defer hub.Close()

	// ---- 下载器 ----
	dl := jobs.NewDownloader(st, &runnerAdapter{srcMgr}, cfg.DownloadConcurrency)
	dl.SetHub(hub)
	dl.SetLogger(func(format string, args ...any) {
		logger.Info("download", "msg", fmt.Sprintf(format, args...))
	})
	// 图片抓取的代理配置：注册进程级读取器，抓取时按需构造 client。
	jshost.SetProxyProvider(func() jshost.ProxyConfig {
		return jshost.ProxyConfig{Raw: cfg.Proxy, Bypass: cfg.ProxyBypass}
	})
	dl.SetFetch(func(url string, headers map[string]string) ([]byte, error) {
		// 图片下载专用：跟随重定向（图床 301 跳 CDN 是常态，跨 host
		// 重定向被拦曾导致 picacg 封面全数下载失败），代理配置由
		// SetProxyProvider 注册的进程级读取器提供。
		return jshost.FetchImageWithProxy(nil, url, headers)
	})
	dl.SetTransform(func(initJS string, cfg *jobs.ImageConfig, data []byte) ([]byte, error) {
		if initJS == "" {
			initJS = assets.InitJS
		}
		if cfg.OnResponseJS != "" {
			out, err := transformOnResponse(initJS, cfg.OnResponseJS, data)
			if err != nil {
				return nil, err
			}
			data = out
		}
		if cfg.ModifyImageJS != "" {
			out, err := transformModifyImage(initJS, cfg.ModifyImageJS, data)
			if err != nil {
				return nil, err
			}
			data = out
		}
		return data, nil
	})

	// ---- 任务 worker ----
	worker := jobs.NewWorker(st, &runnerAdapter{srcMgr}, cfg.Workers)
	worker.SetDownloader(dl)
	worker.SetLogger(func(format string, args ...any) {
		logger.Info("worker", "msg", fmt.Sprintf(format, args...))
	})

	// 收藏新增 → 自动排队下载
	if cfg.AutoDownloadFavorites {
		syncEngine.OnNewFavorites = func(items []model.Favorite) error {
			for _, f := range items {
				_, err := worker.Enqueue(f.Source, f.ID, f.Title, "favorite-sync", nil)
				if err == nil {
					continue
				}
				// 已在队列 / 已缓存都是正常情况：客户端每次启动都会
				// 全量重推收藏，这里必须安静跳过，不能刷警告。
				if errors.Is(err, jobs.ErrAlreadyQueued) ||
					errors.Is(err, jobs.ErrAlreadyCached) {
					continue
				}
				logger.Warn("enqueue favorite failed", "source", f.Source, "id", f.ID, "err", err)
			}
			return nil
		}
	}

	// ---- 源脚本同步 ----
	//
	// 语义是"客户端装了什么，服务端就持有什么"：
	//   - 客户端把本地已安装的源脚本推上来（source.script）；
	//   - 客户端卸载某个源时推 source.remove，服务端跟着删；
	//   - 服务端从不主动全量导入 venera-configs，
	//     否则会凭空多出用户根本没启用的几十个源，下载队列也会被撑大。
	syncEngine.OnSourceScript = func(payload model.SourceScriptOpPayload) error {
		if err := srcMgr.InstallByKey(payload.Source, payload.Script); err != nil {
			logger.Warn("install source failed", "source", payload.Source, "err", err)
			return err
		}
		logger.Info("source synced from client",
			"source", payload.Source,
			"version", payload.Version,
			"bytes", len(payload.Script))
		return nil
	}
	syncEngine.OnSourceRemove = func(sourceKey string) error {
		if err := srcMgr.Delete(sourceKey); err != nil && !errors.Is(err, source.ErrSourceNotFound) {
			logger.Warn("remove source failed", "source", sourceKey, "err", err)
			return err
		}
		logger.Info("source removed by client", "source", sourceKey)
		return nil
	}
	// 快照里的源清单以磁盘实际扫描结果为准
	syncEngine.ListInstalledSources = func() []store.SourceInfo {
		list := srcMgr.List()
		out := make([]store.SourceInfo, 0, len(list))
		for _, s := range list {
			out = append(out, store.SourceInfo{
				Key: s.Key, Name: s.Name, Version: s.Version, Enabled: true,
			})
		}
		return out
	}

	// ---- API ----
	srv := api.New(api.Deps{
		Config: api.Config{
			Token:         cfg.Token,
			PublicBaseURL: cfg.PublicBaseURL,
			LibraryDir:    cfg.LibraryDir,
			DataDir:       cfg.DataDir,
			Version:       version,
			EnableWebDAV:  true,
			WebDAVUser:    envOr("VENERA_WEBDAV_USER", "venera"),
			WebDAVPass:    envOr("VENERA_WEBDAV_PASS", "venera"),
		},
		Store:  st,
		Sync:   syncEngine,
		Source: srcMgr,
		Worker: worker,
		DL:     dl,
		Hub:    hub,
		Logger: logger,
	})

	// 一次性导入模式（迁移旧数据）
	if doImport {
		if archive == "" {
			return errors.New("-import requires -archive <path>")
		}
		return srv.ImportVeneraArchive(archive)
	}

	// ---- 启动 ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 下载大图/长请求场景给足时间；具体写超时由 handler 控制
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		// 代理是排查"某些源一直 dial tcp i/o timeout"的第一手信息：
		// 没配代理时 ehentai/picacg 这类站点在国内网络下会直接超时，
		// 而错误信息只写 "dial tcp ..."，看不出到底有没有走代理。
		proxyDesc := cfg.Proxy
		if proxyDesc == "" || strings.EqualFold(proxyDesc, "none") ||
			strings.EqualFold(proxyDesc, "direct") {
			proxyDesc = "(direct)"
		}
		logger.Info("listening", "addr", cfg.Listen, "version", version,
			"proxy", proxyDesc)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case s := <-sig:
		logger.Info("shutting down", "signal", s.String())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	// 先停 HTTP（不再接新请求），再停 worker（让进行中的任务收尾）
	_ = httpSrv.Shutdown(shutdownCtx)
	worker.Stop()
	logger.Info("bye")
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// storeAdapter 把 store.Store 适配成 jshost.SourceDataStore。
type storeAdapter struct{ *store.Store }

func (s *storeAdapter) GetSourceData(k string) (map[string]any, error) {
	return s.Store.GetSourceData(k)
}
func (s *storeAdapter) MergeSourceData(k string, p map[string]any) error {
	return s.Store.MergeSourceData(k, p)
}
func (s *storeAdapter) GetCookies(k string) ([]model.CookieRecord, error) {
	return s.Store.GetCookies(k)
}
func (s *storeAdapter) PutCookies(k string, c []model.CookieRecord) error {
	return s.Store.PutCookies(k, c)
}

// runnerAdapter 把 source.Manager 适配成 jobs.SourceRunner。
type runnerAdapter struct{ m *source.Manager }

func (r *runnerAdapter) LoadDetail(s, c string) (*model.ComicDetail, error) {
	return r.m.LoadDetail(s, c)
}
func (r *runnerAdapter) LoadEpisodes(s, c, e string) ([]string, error) {
	return r.m.LoadEpisodes(s, c, e)
}
func (r *runnerAdapter) ImageConfigFor(s, u, c, e string) (*jobs.ImageConfig, error) {
	cfg, err := r.m.ImageConfigFor(s, u, c, e)
	if err != nil || cfg == nil {
		return nil, err
	}
	return &jobs.ImageConfig{
		URL: cfg.URL, Method: cfg.Method, Data: cfg.Data, Headers: cfg.Headers,
		OnResponseJS: cfg.OnResponseJS, ModifyImageJS: cfg.ModifyImageJS,
	}, nil
}

var _ = strings.TrimSpace
