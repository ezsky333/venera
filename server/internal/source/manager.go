// Package source 管理已安装的漫画源，并在独立的 JS VM 上执行它们的方法。
package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/venera-app/venera-nas/internal/jshost"
	"github.com/venera-app/venera-nas/internal/model"
)

// ErrSourceNotFound 表示请求的源未安装。
var ErrSourceNotFound = errors.New("source not found")

// Options 是 Manager 的构造参数。
type Options struct {
	DataDir string
	Store   jshost.SourceDataStore
	// InitJS 是 venera 的 assets/init.js 内容。
	InitJS   string
	Logger   func(level, title, content string)
	Limiter  jshost.RateLimiter
	RPS      float64
	Timeout  time.Duration
	Platform string
	Locale   string
	// Proxy 是访问源站点的出口代理（NAS 自身 API 不受影响）
	Proxy jshost.ProxyConfig
}

// Manager 负责源的安装、索引与执行。
//
// 每次执行都会新建一个 Host（全新 JS VM）。原因：
//  1. 源之间共享全局状态会互相污染；
//  2. 单次长跑可能泄漏内存，用完即弃最稳；
//  3. 我们按任务并发，不需要 VM 复用。
//
// 代价约 3-8ms/次初始化，相对网络 IO 可忽略。
type Manager struct {
	dataDir string
	mu      sync.RWMutex
	scripts map[string]scriptEntry

	initJS  string
	store   jshost.SourceDataStore
	limiter jshost.RateLimiter
	logf    func(level, title, content string)
	opts    Options
}

type scriptEntry struct {
	Key     string
	Name    string
	Version string
	Path    string
	JS      string
}

// NewManager 创建源管理器并扫描已安装脚本。
func NewManager(opts Options) (*Manager, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 150 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = func(level, title, content string) {}
	}
	m := &Manager{
		dataDir: opts.DataDir,
		scripts: map[string]scriptEntry{},
		initJS:  opts.InitJS,
		store:   opts.Store,
		logf:    opts.Logger,
		opts:    opts,
		limiter: opts.Limiter,
	}
	if m.limiter == nil && opts.RPS > 0 {
		m.limiter = jshost.NewHostLimiter(opts.RPS)
	}
	if err := m.Scan(); err != nil {
		return nil, err
	}
	return m, nil
}

// Scan 重新扫描 sources 目录。
func (m *Manager) Scan() error {
	dir := filepath.Join(m.dataDir, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	next := map[string]scriptEntry{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			m.logf("error", "source", fmt.Sprintf("read %s: %v", e.Name(), err))
			continue
		}
		js := string(b)
		key, name, version, err := parseMeta(js)
		if err != nil {
			m.logf("warning", "source", fmt.Sprintf("skip %s: %v", e.Name(), err))
			continue
		}
		next[key] = scriptEntry{Key: key, Name: name, Version: version, Path: path, JS: js}
	}

	m.mu.Lock()
	m.scripts = next
	m.mu.Unlock()

	// 把磁盘扫描结果登记到 sources 表。
	//
	// 必要性：/api/v1/sources 与快照都读这张表。若只在 Install/InstallByKey
	// 里登记，服务端重启后（内存索引从磁盘重建、数据库却还是旧的）
	// 会出现"源明明在跑、接口返回 0 个源"的不一致。
	if rec, ok := m.store.(SourceMetaStore); ok {
		for _, s := range next {
			_ = rec.PutSource(s.Key, s.Name, s.Version, filepath.Base(s.Path))
		}
	}
	return nil
}

// parseMeta 从脚本文本中提取 key/name/version。
//
// 这里刻意不执行 JS：源可能来自网络，加载即执行会带来安全风险。
// 代价是解析是启发式的，因此失败时只跳过该源，不影响其他源。
func parseMeta(js string) (key, name, version string, err error) {
	key = firstStringField(js, "key")
	name = firstStringField(js, "name")
	version = firstStringField(js, "version")
	if key == "" {
		return "", "", "", fmt.Errorf("missing key field")
	}
	if name == "" {
		name = key
	}
	if version == "" {
		version = "0.0.0"
	}
	return key, name, version, nil
}

func firstStringField(js, field string) string {
	for _, sep := range []string{"=", ":"} {
		pat := field + " " + sep
		idx := 0
		for idx < len(js) {
			i := strings.Index(js[idx:], pat)
			if i < 0 {
				break
			}
			i += idx
			prevOK := i == 0 || js[i-1] == '\n' || js[i-1] == '\t' || js[i-1] == ' ' || js[i-1] == '\r'
			if prevOK {
				rest := strings.TrimLeft(js[i+len(pat):], " \t")
				if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'' || rest[0] == '`') {
					q := rest[0]
					if end := strings.IndexByte(rest[1:], q); end >= 0 {
						v := rest[1 : end+1]
						if !strings.Contains(v, "${") {
							return v
						}
					}
				}
			}
			idx = i + len(pat)
		}
	}
	return ""
}

// Install 写入源脚本并刷新索引。
func (m *Manager) Install(fileName, js string) error {
	if !strings.HasSuffix(fileName, ".js") {
		fileName += ".js"
	}
	fileName = filepath.Base(fileName) // 防路径穿越
	key, name, version, err := parseMeta(js)
	if err != nil {
		return err
	}
	dir := filepath.Join(m.dataDir, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(js), 0o644); err != nil {
		return err
	}
	m.mu.Lock()
	m.scripts[key] = scriptEntry{Key: key, Name: name, Version: version, Path: path, JS: js}
	m.mu.Unlock()
	return nil
}

// InstallByKey 以源自身声明的 key 为文件名写入脚本。
//
// 与 Install 的区别：文件名不用 key（可能含路径分隔符），
// 而是统一用 `<key>.js`，并对不安全字符做替换——
// 这个路径来自另一端设备上送的 op，必须当成不可信输入。
func (m *Manager) InstallByKey(key, js string) error {
	if key == "" {
		return fmt.Errorf("empty source key")
	}
	safe := sanitizeFileName(key)
	dir := filepath.Join(m.dataDir, "sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// 先解析元信息：脚本语法/结构不对就不落盘，避免污染源目录
	parsedKey, name, version, perr := parseMeta(js)
	if perr != nil {
		return fmt.Errorf("parse source %q: %w", key, perr)
	}
	// key 比对**不区分大小写**，并以脚本自己声明的写法为准。
	//
	// 现实的源里确实存在这种情况：komiic 的脚本声明 key 为 `Komiic`，
	// 而客户端同步时用的是规范化后的小写 `komiic`。严格比对会让这个源
	// 永远装不上（日志是 "source key mismatch: declared \"Komiic\",
	// expected \"komiic\""），用户看到的现象是"别的源都同步了，就它没有"。
	//
	// 存储一律用小写 key：下游的目录名、DB 主键、按 key 查脚本都按小写，
	// 混用两种写法会分裂成两个源。
	key = strings.ToLower(key)
	if !strings.EqualFold(parsedKey, key) {
		return fmt.Errorf("source key mismatch: declared %q, expected %q", parsedKey, key)
	}

	path := filepath.Join(dir, safe+".js")
	if err := os.WriteFile(path, []byte(js), 0o644); err != nil {
		return err
	}
	m.mu.Lock()
	m.scripts[key] = scriptEntry{Key: key, Name: name, Version: version, Path: path, JS: js}
	m.mu.Unlock()

	// 同步登记到数据库：/api/v1/sources 与快照都读这张表，
	// 只更新内存索引会导致"脚本在磁盘上、接口却查不到"。
	if rec, ok := m.store.(SourceMetaStore); ok {
		_ = rec.PutSource(key, name, version, filepath.Base(path))
	}
	return nil
}

// SourceMetaStore 是源元信息的持久化能力（由 store.Store 实现）。
//
// 单独定义接口而不是直接依赖 store 包：source 包只需要这两个方法，
// 引入整个 store 会造成循环依赖风险，也让测试替身变重。
type SourceMetaStore interface {
	PutSource(key, name, version, file string) error
	DeleteSource(key string) error
}

// sanitizeFileName 把 key 转成安全的文件名。
func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "source"
	}
	if len([]rune(out)) > 100 {
		out = string([]rune(out)[:100])
	}
	return out
}

// InstallFromURL 下载并安装源脚本。
func (m *Manager) InstallFromURL(fileName, jsURL string, client jshost.HTTPDoer) error {
	if client == nil {
		client = jshost.DefaultClient()
	}
	req, err := http.NewRequest(http.MethodGet, jsURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", jsURL, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return m.Install(fileName, string(b))
}

// Source 描述一个已安装源。
type Source struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"-"`
}

// Bundle 是分发给客户端的完整源脚本。
//
// 为什么要下发脚本本身而不只是 key/name：
// 新设备（平板/模拟器）刚装好 App 时**一个源都没有**。用户手动导入 34 个
// JS 文件是不现实的，而没有源就没法渲染收藏、没法阅读。
// 既然服务端已经为了下载而持有一份源脚本，直接复用是最省事且一致的方案
// ——两端跑的是同一版本，行为不会分叉。
type Bundle struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Script  string `json:"script"`
}

// Bundles 返回所有源的完整脚本（按 key 排序，保证输出稳定）。
func (m *Manager) Bundles() []Bundle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Bundle, 0, len(m.scripts))
	for _, s := range m.scripts {
		out = append(out, Bundle{
			Key: s.Key, Name: s.Name, Version: s.Version, Script: s.JS,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Bundle 返回单个源的脚本。
func (m *Manager) Bundle(key string) (Bundle, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.scripts[key]
	if !ok {
		return Bundle{}, false
	}
	return Bundle{Key: s.Key, Name: s.Name, Version: s.Version, Script: s.JS}, true
}

// List 返回所有已安装源（按 key 排序以保证输出稳定）。
func (m *Manager) List() []Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Source, 0, len(m.scripts))
	for _, s := range m.scripts {
		out = append(out, Source{Key: s.Key, Name: s.Name, Version: s.Version, Path: s.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Has 判断源是否已安装。
func (m *Manager) Has(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.scripts[key]
	return ok
}

// Name 返回源显示名。
func (m *Manager) Name(key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.scripts[key]; ok {
		return e.Name
	}
	return key
}

// Delete 删除源脚本。
func (m *Manager) Delete(key string) error {
	m.mu.Lock()
	e, ok := m.scripts[key]
	if ok {
		delete(m.scripts, key)
	}
	m.mu.Unlock()
	if !ok {
		return ErrSourceNotFound
	}
	if rec, ok := m.store.(SourceMetaStore); ok {
		_ = rec.DeleteSource(key)
	}
	return os.Remove(e.Path)
}

// withHost 创建一个 Host，执行 fn，然后销毁。
// fn 在独立 goroutine 中运行，可安全调用 h.Call。
func (m *Manager) withHost(key string, fn func(h *jshost.Host) error) error {
	m.mu.RLock()
	entry, ok := m.scripts[key]
	m.mu.RUnlock()
	if !ok {
		return ErrSourceNotFound
	}

	opts := jshost.Options{
		SourceKey:  key,
		InitJS:     m.initJS,
		SourceJS:   entry.JS,
		Store:      m.store,
		Logger:     m.logf,
		Timeout:    m.opts.Timeout,
		Platform:   m.opts.Platform,
		Locale:     m.opts.Locale,
		HTTPClient: jshost.DefaultClientWithProxy(m.opts.Proxy),
		Limiter:    m.limiter,
	}

	h, err := jshost.New(opts)
	if err != nil {
		return fmt.Errorf("new host for %s: %w", key, err)
	}
	defer h.Close()

	// 一个 Host 只被一个 goroutine 使用；RunLoop 与 fn 都在这里串行执行，
	// 因为 fn 内部只会阻塞等待，不会并发访问 VM。
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		h.RunLoop()
	}()
	defer func() {
		h.Close()
		<-loopDone
	}()

	// 复刻 App 的 js_engine.init()：源实例化后先调用其 init()，
	// 成功后才算可用。见 runInit。
	if err := m.runInit(h); err != nil {
		return err
	}
	return fn(h)
}

// runInit 调用源的 init() 钩子（若存在）。
//
// 这一步不是可选的装饰：jm/wnacg 这类源把域名列表放在 init() 里异步拉取，
// 没跑 init 就直接 loadInfo，baseUrl getter 取到 undefined 下标，
// 报的却是 "Cannot read property '1' of undefined"（真正的空指针在下标处）。
//
// 失败语义（与 App 一致）：
//   - init 抛错 → 该源视为加载失败，调用方直接拿到错误；
//   - 源没有 init 或 init 非函数 → 视为成功。
func (m *Manager) runInit(h *jshost.Host) error {
	v, err := h.Call(`(async () => {
		const s = __source;
		if (!s || typeof s.init !== "function") return "skip";
		await s.init();
		return "ok";
	})()`)
	if err != nil {
		return fmt.Errorf("source init: %w", err)
	}
	_ = v
	return nil
}

// ---------- 源方法封装 ----------

// LoadDetail 调用 comic.loadInfo(id)。
func (m *Manager) LoadDetail(key, comicID string) (*model.ComicDetail, error) {
	var out *model.ComicDetail
	err := m.withHost(key, func(h *jshost.Host) error {
		// 关键：在 JS 内就把章节转成有序数组。
		//
		// 两个必须处理的陷阱：
		//  1. 有些源（如 manga_dex）的 chapters 是 **JS Map** 而非普通对象，
		//     JSON.stringify(Map) 会得到 "{}"。必须判断并转成 entries。
		//  2. Go 的 map 遍历无序，因此顺序必须在 JS 侧固定下来。
		//  3. 有些源会给 Map/对象挂附加属性（如 latestChapterMarker），
		//     必须只取真正的 entry，不能当成章节。
		expr := fmt.Sprintf(`(async () => {
			const d = await __source.comic.loadInfo(%s);
			const chs = d && d.chapters;
			const chapters = [];
			let grouped = false;
			const entriesOf = (c) => {
				if (!c) return [];
				if (c instanceof Map) return Array.from(c.entries());
				if (typeof c === "object") return Object.entries(c);
				return [];
			};
			const outer = entriesOf(chs);
			// 判断是否分组：值若是对象/Map 则为分组
			for (const [k, v] of outer) {
				if (v !== null && typeof v === "object") grouped = true;
				break;
			}
			for (const [g, v] of outer) {
				if (grouped) {
					for (const [k, t] of entriesOf(v)) {
						chapters.push({id: String(k), title: String(t), group: String(g)});
					}
				} else {
					chapters.push({id: String(g), title: String(v), group: ""});
				}
			}
			return JSON.stringify({
				title: d.title || "", subtitle: d.subtitle || d.subTitle || "",
				cover: d.cover || "", description: d.description || "",
				tags: d.tags || {}, maxPage: d.maxPage === undefined ? null : d.maxPage,
				chapters
			});
		})()`, jsString(comicID))

		v, err := h.Call(expr)
		if err != nil {
			return err
		}
		var payload struct {
			Title       string `json:"title"`
			Subtitle    string `json:"subtitle"`
			Cover       string `json:"cover"`
			Description string `json:"description"`
			// tags 在不同源里既可能是 map[string][]string，也可能是
			// map[string]string 或扁平数组，这里用 RawMessage 兼容。
			Tags    json.RawMessage `json:"tags"`
			MaxPage *int            `json:"maxPage"`
			Chapters []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Group string `json:"group"`
			} `json:"chapters"`
		}
		if err := json.Unmarshal([]byte(v.String()), &payload); err != nil {
			return fmt.Errorf("decode loadInfo: %w", err)
		}

		d := &model.ComicDetail{
			Comic: model.Comic{
				Source: key, ID: comicID, Title: payload.Title,
				Subtitle: payload.Subtitle, Cover: payload.Cover, MaxPage: payload.MaxPage,
			},
			Description: payload.Description,
		}
		d.Comic.Tags = flattenTags(payload.Tags)
		for _, c := range payload.Chapters {
			d.Chapters = append(d.Chapters, model.Chapter{ID: c.ID, Title: c.Title, Group: c.Group})
		}
		d.Grouped = len(d.Chapters) > 0 && d.Chapters[0].Group != ""
		out = d
		return nil
	})
	return out, err
}

// flattenTags 把源的 tags 字段（形态多样）统一成 "namespace:tag" 列表。
//
// 已观察到的形态：
//   - {"Tags": ["a","b"], "Authors": ["x"]}
//   - {"Tags": "a, b"}            (逗号分隔字符串)
//   - ["a","b"]                   (扁平数组)
func flattenTags(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []string

	// 扁平数组
	var flat []string
	if err := json.Unmarshal(raw, &flat); err == nil {
		out = append(out, flat...)
		sort.Strings(out)
		return out
	}

	// map 形态
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err == nil {
		keys := make([]string, 0, len(asMap))
		for k := range asMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, ns := range keys {
			var arr []string
			if err := json.Unmarshal(asMap[ns], &arr); err == nil {
				for _, t := range arr {
					out = append(out, ns+":"+t)
				}
				continue
			}
			var s string
			if err := json.Unmarshal(asMap[ns], &s); err == nil && s != "" {
				for _, t := range strings.Split(s, ",") {
					if t = strings.TrimSpace(t); t != "" {
						out = append(out, ns+":"+t)
					}
				}
			}
		}
	}
	return out
}

// LoadEpisodes 调用 comic.loadEp(id, ep)，返回图片 URL 列表。
func (m *Manager) LoadEpisodes(key, comicID, ep string) ([]string, error) {
	var urls []string
	err := m.withHost(key, func(h *jshost.Host) error {
		epArg := "null"
		if ep != "" {
			epArg = jsString(ep)
		}
		expr := fmt.Sprintf(`(async () => {
			const r = await __source.comic.loadEp(%s, %s);
			const images = (r && r.images) ? r.images : r;
			return JSON.stringify(Array.isArray(images) ? images : []);
		})()`, jsString(comicID), epArg)

		v, err := h.Call(expr)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(v.String()), &urls); err != nil {
			return fmt.Errorf("decode loadEp: %w", err)
		}
		return nil
	})
	return urls, err
}

// ImageConfig 是源为某张图片指定的加载方式（对应 App 的 ImageLoadingConfig）。
type ImageConfig struct {
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Data    any               `json:"data,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// OnResponseJS 是字节级变换函数源码（异或/解密）。
	OnResponseJS string `json:"onResponseJS,omitempty"`
	// ModifyImageJS 是图片级变换函数源码（切片重排）。
	ModifyImageJS string `json:"modifyImageJS,omitempty"`
}

// ImageConfigFor 取图片加载配置（对应 App 的 ImageLoadingConfig）。
//
// 这是下载链路上最易出错的一环：不同源用不同方式鉴权/解密。
//
// 与 App 的对齐点（lib/foundation/comic_source/parser.dart
// _parseImageLoadingConfigFunc）：源把配置函数挂在 **comic.onImageLoad**，
// 调用形如 comic.onImageLoad(url, comicId, ep)。jm 的切片重排
// （modifyImage）与图床请求头都从这里给出——早期实现误调
// s.getImageLoadingConfig（不存在），导致 jm 全部长图按原始
// 打乱顺序落盘，阅读时满屏错位横条。
func (m *Manager) ImageConfigFor(key, imageURL, comicID, ep string) (*ImageConfig, error) {
	var cfg *ImageConfig
	err := m.withHost(key, func(h *jshost.Host) error {
		expr := fmt.Sprintf(`(async () => {
			const s = __source;
			const out = {};
			const fn = s.comic && s.comic.onImageLoad;
			if (typeof fn === "function") {
				const c = await fn(%s, %s, %s);
				if (c) {
					if (c.url) out.url = String(c.url);
					if (c.method) out.method = String(c.method);
					if (c.data !== undefined && c.data !== null) out.data = c.data;
					if (c.headers) {
						const hh = {};
						for (const k of Object.keys(c.headers)) hh[k] = String(c.headers[k]);
						out.headers = hh;
					}
					if (typeof c.onResponse === "function") out.onResponseJS = c.onResponse.toString();
					if (c.modifyImage) out.modifyImageJS = String(c.modifyImage);
				}
			}
			return JSON.stringify(out);
		})()`, jsString(imageURL), jsString(comicID), jsString(ep))

		v, err := h.Call(expr)
		if err != nil {
			return err
		}
		var payload ImageConfig
		if err := json.Unmarshal([]byte(v.String()), &payload); err != nil {
			return fmt.Errorf("decode image config: %w", err)
		}
		cfg = &payload
		return nil
	})
	return cfg, err
}

// Favorites 拉取源上的网络收藏（需已登录）。folder 为空表示默认收藏夹。
func (m *Manager) Favorites(key, folder string, maxPages int) ([]model.Comic, error) {
	var out []model.Comic
	err := m.withHost(key, func(h *jshost.Host) error {
		if maxPages <= 0 {
			maxPages = 5
		}
		folderArg := "null"
		if folder != "" {
			folderArg = jsString(folder)
		}
		expr := fmt.Sprintf(`(async () => {
			const s = __source;
			if (!s.favorites || !s.favorites.loadComics) return "[]";
			const all = [];
			let page = 1;
			while (page <= %d) {
				const r = await s.favorites.loadComics(page, %s);
				const list = (r && r.comics) ? r.comics : r;
				if (!list || !list.length) break;
				for (const c of list) {
					all.push({
						id: String(c.id), title: c.title || "",
						cover: c.cover || "",
						subtitle: c.subtitle || c.subTitle || "",
						tags: c.tags || []
					});
				}
				const mp = r && r.maxPage;
				if (mp && page >= mp) break;
				page++;
			}
			return JSON.stringify(all);
		})()`, maxPages, folderArg)

		v, err := h.Call(expr)
		if err != nil {
			return err
		}
		var raw []struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			Cover    string   `json:"cover"`
			Subtitle string   `json:"subtitle"`
			Tags     []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(v.String()), &raw); err != nil {
			return fmt.Errorf("decode favorites: %w", err)
		}
		for _, r := range raw {
			out = append(out, model.Comic{
				Source: key, ID: r.ID, Title: r.Title,
				Cover: r.Cover, Subtitle: r.Subtitle, Tags: r.Tags,
			})
		}
		return nil
	})
	return out, err
}

// Login 调用源的 account.login(username, password)，成功后数据由源自行 saveData。
func (m *Manager) Login(key, user, pass string) error {
	return m.withHost(key, func(h *jshost.Host) error {
		expr := fmt.Sprintf(`(async () => {
			const s = __source;
			if (!s.account || !s.account.login) return JSON.stringify({error: true, errorMessage: "source has no login"});
			try {
				const r = await s.account.login(%s, %s);
				return JSON.stringify({error: !!(r && r.error), errorMessage: (r && r.errorMessage) || ""});
			} catch (e) {
				return JSON.stringify({error: true, errorMessage: String(e)});
			}
		})()`, jsString(user), jsString(pass))

		v, err := h.Call(expr)
		if err != nil {
			return err
		}
		var res struct {
			Error        bool   `json:"error"`
			ErrorMessage string `json:"errorMessage"`
		}
		if err := json.Unmarshal([]byte(v.String()), &res); err != nil {
			return err
		}
		if res.Error {
			return fmt.Errorf("%s", res.ErrorMessage)
		}
		return nil
	})
}

// HasFeature 探测源是否具备某能力（用于 UI 展示与降级决策）。
func (m *Manager) HasFeature(key, feature string) (bool, error) {
	var has bool
	err := m.withHost(key, func(h *jshost.Host) error {
		expr := fmt.Sprintf(`(() => {
			const s = __source;
			const parts = %s.split(".");
			let cur = s;
			for (const p of parts) { if (cur === undefined || cur === null) return "false"; cur = cur[p]; }
			return (typeof cur === "function") ? "true" : "false";
		})()`, jsString(feature))
		v, err := h.Call(expr)
		if err != nil {
			return err
		}
		has = v.String() == "true"
		return nil
	})
	return has, err
}

// jsString 把 Go 字符串安全地嵌入 JS 字面量。
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
