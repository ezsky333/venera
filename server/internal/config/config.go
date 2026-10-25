package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是服务端的全部可调参数。全部支持环境变量覆盖，
// 便于在 NAS 的 Docker Compose 中直接配置，无需挂载配置文件。
type Config struct {
	// Listen 是 HTTP 监听地址，例如 ":8080"。
	Listen string

	// DataDir 存放 SQLite 数据库、源脚本、同步包归档。
	DataDir string

	// LibraryDir 是漫画文件的实际落盘目录（建议挂载 NAS 存储卷）。
	LibraryDir string

	// Token 是客户端访问 API 的共享密钥。为空则不鉴权（仅建议内网使用）。
	Token string

	// PublicBaseURL 用于生成客户端可直接访问的绝对 URL（反代场景）。
	// 为空时按请求的 Host 推断。
	PublicBaseURL string

	// SourceListURL 是 venera-configs 的 index.json 地址。
	SourceListURL string

	// Workers 是同时运行漫画源的并发数。每个 worker 独立持有一个 JS VM。
	Workers int

	// DownloadConcurrency 是单个漫画任务内部的图片并发下载数。
	DownloadConcurrency int

	// GlobalRPS 是所有对外请求的全局速率上限（每站点独立令牌桶）。
	GlobalRPS float64

	// SyncRetention 是同步操作日志的保留时长，超期可安全裁剪。
	SyncRetention time.Duration

	// AutoDownloadFavorites 表示同步到收藏后是否自动排队下载。
	AutoDownloadFavorites bool

	// MaxLibraryBytes 是库大小上限（0 表示不限制）。
	MaxLibraryBytes int64
}

func Default() Config {
	return Config{
		Listen:                ":8080",
		DataDir:               "/data",
		LibraryDir:            "/library",
		SourceListURL:         "https://cdn.jsdelivr.net/gh/venera-app/venera-configs@main/index.json",
		Workers:               3,
		DownloadConcurrency:   4,
		GlobalRPS:             3,
		SyncRetention:         90 * 24 * time.Hour,
		AutoDownloadFavorites: true,
	}
}

// Load 读取配置：默认值 < 环境变量。
// 也支持 VENERA_CONFIG_FILE 指向一个 KEY=VALUE 文件（Docker secrets 场景）。
func Load() (Config, error) {
	c := Default()

	if path := os.Getenv("VENERA_CONFIG_FILE"); path != "" {
		if err := applyFile(&c, path); err != nil {
			return c, err
		}
	}
	applyEnv(&c)

	if err := c.Validate(); err != nil {
		return c, err
	}
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return c, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(c.LibraryDir, 0o755); err != nil {
		return c, fmt.Errorf("create library dir: %w", err)
	}
	return c, nil
}

func applyFile(c *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		os.Setenv("VENERA_"+strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v))
	}
	return nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("VENERA_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("VENERA_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("VENERA_LIBRARY_DIR"); v != "" {
		c.LibraryDir = v
	}
	if v := os.Getenv("VENERA_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("VENERA_PUBLIC_BASE_URL"); v != "" {
		c.PublicBaseURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("VENERA_SOURCE_LIST_URL"); v != "" {
		c.SourceListURL = v
	}
	if v, err := envInt("VENERA_WORKERS"); err == nil && v > 0 {
		c.Workers = v
	}
	if v, err := envInt("VENERA_DOWNLOAD_CONCURRENCY"); err == nil && v > 0 {
		c.DownloadConcurrency = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("VENERA_GLOBAL_RPS"), 64); err == nil && v > 0 {
		c.GlobalRPS = v
	}
	if v, err := envInt("VENERA_SYNC_RETENTION_DAYS"); err == nil && v >= 0 {
		c.SyncRetention = time.Duration(v) * 24 * time.Hour
	}
	if v, err := strconv.ParseBool(os.Getenv("VENERA_AUTO_DOWNLOAD_FAVORITES")); err == nil {
		c.AutoDownloadFavorites = v
	}
	if v, err := strconv.ParseInt(os.Getenv("VENERA_MAX_LIBRARY_BYTES"), 10, 64); err == nil && v > 0 {
		c.MaxLibraryBytes = v
	}
}

func envInt(key string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
}

func (c Config) Validate() error {
	if c.Workers <= 0 {
		return fmt.Errorf("workers must be > 0")
	}
	if c.DownloadConcurrency <= 0 {
		return fmt.Errorf("download concurrency must be > 0")
	}
	if filepath.Clean(c.DataDir) == filepath.Clean(c.LibraryDir) {
		return fmt.Errorf("data dir and library dir must differ")
	}
	return nil
}
