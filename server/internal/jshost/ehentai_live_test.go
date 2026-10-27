package jshost

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/model"
)

// realCookieStore 从真实 nas.db 读 ehentai cookie，用于复现线上问题。
type realCookieStore struct{ db *sql.DB }

func (s *realCookieStore) GetSourceData(string) (map[string]any, error) { return map[string]any{}, nil }
func (s *realCookieStore) MergeSourceData(string, map[string]any) error { return nil }
func (s *realCookieStore) GetCookies(key string) ([]model.CookieRecord, error) {
	rows, err := s.db.Query(
		`SELECT name, value, domain, path, expires, secure, http_only
		 FROM source_cookies WHERE source_key = ?`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CookieRecord
	for rows.Next() {
		var name, value, domain, path string
		var expires sql.NullInt64
		var secure, httpOnly int
		if err := rows.Scan(&name, &value, &domain, &path, &expires, &secure, &httpOnly); err != nil {
			return nil, err
		}
		c := model.CookieRecord{Name: name, Value: value, Domain: domain, Path: path}
		if expires.Valid && expires.Int64 > 0 {
			e := expires.Int64
			c.Expires = &e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *realCookieStore) PutCookies(string, []model.CookieRecord) error { return nil }

// newLiveEhentaiHost 造一个连真实 nas.db 的 Host（Cookie + 代理都就绪）。
// 环境不具备时返回 ok=false，调用方应跳过而不是失败。
func newLiveEhentaiHost(t *testing.T) (*Host, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:D:/MyProject/venera/_runtime/data/nas.db?mode=ro")
	if err != nil {
		t.Skipf("no live db: %v", err)
		return nil, false
	}
	store := &realCookieStore{db: db}
	cks, err := store.GetCookies("ehentai")
	if err != nil || len(cks) == 0 {
		db.Close()
		t.Skipf("no ehentai cookies (err=%v n=%d)", err, len(cks))
		return nil, false
	}
	h, err := New(Options{
		SourceKey:  "ehentai",
		InitJS:     assets.InitJS,
		SourceJS:   `class _T extends ComicSource { key = "ehentai"; }`,
		Store:      store,
		Timeout:    60 * 1e9,
		HTTPClient: DefaultClientWithProxy(ProxyConfig{Raw: "http://127.0.0.1:7890"}),
	})
	if err != nil {
		db.Close()
		t.Logf("new host failed: %v", err)
		return nil, false
	}
	// db 由 host 生命周期外持有，测试结束后由进程回收（只读连接）
	t.Cleanup(func() { db.Close() })
	return h, true
}

// TestEhentaiGalleryPageHasToken 复现线上 ehentai 失败：
//
//	源脚本 loadInfo 里 `document.querySelectorAll("script").find(e => e.text.includes("var token"))`
//	返回 undefined → `script.text` 抛 "Cannot read property 'text' of undefined"。
//	根因有两层：Cookie/网络层正常（见本测试），DOM 层曾把 script 的
//	.text 一律当作空串（见 TestScriptElementTextIsReadable）。
func TestEhentaiGalleryPageHasToken(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	h, ok := newLiveEhentaiHost(t)
	if !ok {
		return
	}
	defer h.Close()
	go h.RunLoop()

	url := "https://exhentai.org/g/2643494/a63e95900d/"
	v, err := h.Call(`(async () => {
		const res = await Network.get("` + url + `");
		return JSON.stringify({status: res.status, len: res.body.length,
			hasToken: res.body.includes("var token"),
			head: res.body.slice(0, 160)});
	})()`)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	t.Logf("result: %s", v.String())

	if !strings.Contains(v.String(), `"status":200`) {
		t.Fatalf("expected 200, got %s", v.String())
	}
	if !strings.Contains(v.String(), `"hasToken":true`) {
		t.Fatalf("page has no 'var token' -> loadInfo will crash: %s", v.String())
	}
}
