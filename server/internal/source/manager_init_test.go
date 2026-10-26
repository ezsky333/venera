package source

import (
	"strings"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/model"
)

// initTestStore 是最小的 SourceDataStore。
type initTestStore struct{ d map[string]map[string]any }

func (m *initTestStore) GetSourceData(k string) (map[string]any, error) {
	if v, ok := m.d[k]; ok {
		return v, nil
	}
	return map[string]any{}, nil
}
func (m *initTestStore) MergeSourceData(k string, p map[string]any) error {
	if m.d == nil {
		m.d = map[string]map[string]any{}
	}
	if m.d[k] == nil {
		m.d[k] = map[string]any{}
	}
	for kk, vv := range p {
		m.d[k][kk] = vv
	}
	return nil
}
func (m *initTestStore) GetCookies(string) ([]model.CookieRecord, error) { return nil, nil }
func (m *initTestStore) PutCookies(string, []model.CookieRecord) error  { return nil }

// newInitTestManager 构造只含一个源的管理器。
func newInitTestManager(t *testing.T, key, js string, store *initTestStore) *Manager {
	t.Helper()
	m, err := NewManager(Options{
		DataDir:  t.TempDir(),
		Store:    store,
		InitJS:   assets.InitJS,
		Timeout:  10 * time.Second,
		Platform: "linux",
		Locale:   "zh_CN",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	// 绕过 Scan：直接把脚本塞进索引，避免依赖磁盘布局
	m.scripts[key] = scriptEntry{Key: key, Name: key, Version: "1.0.0", JS: js}
	return m
}

// TestWithHostCallsSourceInit 锁住这个真实 bug：
//
//	App 的 js_engine 在实例化源之后会调用 `init()`。jm 与 wnacg 这类源把
//	域名列表的拉取放在 init() 里；不调用它，`JM.apiDomains` 永远是
//	undefined，baseUrl getter 取到 `undefined[0]`，报出来的却是
//	"TypeError: Cannot read property '1' of undefined"（真正的空指针在下标）。
//
// 服务端早期版本没有这一步，导致 jm 全部 66 个收藏、wnacg 全部 24 个收藏
// 在下载时一律失败。
func TestWithHostCallsSourceInit(t *testing.T) {
	js := `
class _InitSpy extends ComicSource {
    key = "_initspy"
    name = "spy"
    version = "1.0.0"

    static marks = []

    async init() {
        _InitSpy.marks.push("init")
        // 模拟真实源：init 里写入后续 getter 依赖的数据
        _InitSpy.domains = ["a.example", "b.example"]
    }

    get baseUrl() {
        // 不跑 init 时这里会抛 TypeError（与 jm 完全同构）
        return "https://" + _InitSpy.domains[0]
    }

    comic = {
        loadInfo: async (id) => {
            return new ComicDetails({
                id: id,
                title: "ok:" + this.baseUrl,
                cover: "",
                chapters: new Map([["1", "第1話"]]),
                tags: new Map(),
                description: "",
            })
        }
    }
}
`
	m := newInitTestManager(t, "_initspy", js, &initTestStore{})
	d, err := m.LoadDetail("_initspy", "42")
	if err != nil {
		t.Fatalf("LoadDetail must succeed after init() runs: %v", err)
	}
	if !strings.Contains(d.Title, "https://a.example") {
		t.Fatalf("init() must have populated domains before loadInfo, got %q", d.Title)
	}
}

// TestWithHostToleratesMissingInit 确认没有 init 的源照常工作。
func TestWithHostToleratesMissingInit(t *testing.T) {
	js := `
class _NoInit extends ComicSource {
    key = "_noinit"
    name = "noinit"
    version = "1.0.0"

    comic = {
        loadInfo: async (id) => new ComicDetails({
            id: id, title: "plain", cover: "",
            chapters: new Map([["1", "c1"]]), tags: new Map(), description: "",
        })
    }
}
`
	m := newInitTestManager(t, "_noinit", js, &initTestStore{})
	d, err := m.LoadDetail("_noinit", "7")
	if err != nil {
		t.Fatalf("source without init() must still work: %v", err)
	}
	if d.Title != "plain" {
		t.Fatalf("unexpected title %q", d.Title)
	}
	if len(d.Chapters) != 1 {
		t.Fatalf("expected 1 chapter, got %d", len(d.Chapters))
	}
}

// TestRunInitIsCalledOnlyOncePerHost 确认 init 不会被重复执行多次
// （重复执行会导致每章都多打一次域名列表请求）。
func TestRunInitIsCalledOncePerCall(t *testing.T) {
	js := `
class _CountInit extends ComicSource {
    key = "_countinit"
    name = "countinit"
    version = "1.0.0"

    static count = 0

    async init() { _CountInit.count++ }

    comic = {
        loadInfo: async (id) => {
            return new ComicDetails({
                id: id, title: "n=" + _CountInit.count, cover: "",
                chapters: new Map([["1", "c1"]]), tags: new Map(), description: "",
            })
        }
    }
}
`
	m := newInitTestManager(t, "_countinit", js, &initTestStore{})
	d, err := m.LoadDetail("_countinit", "1")
	if err != nil {
		t.Fatalf("LoadDetail: %v", err)
	}
	if d.Title != "n=1" {
		t.Fatalf("init() must run exactly once per host, got %q", d.Title)
	}
}
