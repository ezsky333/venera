package jshost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonQuote 把 Go 字符串转成 JS 字面量。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestPostBodyFollowsContentType 锁住 ehentai 全线失败的第二层根因。
//
// 源脚本给 Network.post 传一个 JS 对象 + Content-Type: application/json，
// 期望服务端发出 JSON body。早期实现无视 Content-Type、一律按 URL 表单
// 编码，上游于是回 {"error":"Invalid JSON Request"}：
//   - ehentai 的 showpage / imagedispatch 全部拿不到 i3/i6；
//   - 源脚本 `reg[1]` 抛 Cannot read property '1' of undefined；
//   - 101/107 个 ehentai 任务失败。
//
// dio 的语义是：Content-Type 含 json → jsonEncode；否则表单编码。
// 这个测试把两种分支都钉死。
func TestPostBodyFollowsContentType(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)

	// --- JSON 分支 ---
	_, err := h.Call(`(async () => {
		await Network.post(` + jsonQuote(srv.URL+"/api") + `, {
			'Content-Type': 'application/json'
		}, { gid: "2643494", page: 1, nl: null });
		return "ok";
	})()`)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(gotCT, "json") {
		t.Fatalf("content-type not forwarded: %q", gotCT)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("JSON body expected, got %q (err=%v)", gotBody, err)
	}
	if parsed["gid"] != "2643494" {
		t.Fatalf("field lost in JSON body: %v", parsed)
	}
	if _, ok := parsed["page"]; !ok {
		t.Fatalf("numeric field lost: %v", parsed)
	}

	// --- 表单分支（未声明 json）---
	gotBody = ""
	_, err = h.Call(`(async () => {
		await Network.post(` + jsonQuote(srv.URL+"/form") + `, {}, { a: "1", b: "2" });
		return "ok";
	})()`)
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	if strings.Contains(gotBody, "{") {
		t.Fatalf("form branch must not send JSON, got %q", gotBody)
	}
	if !strings.Contains(gotBody, "a=1") || !strings.Contains(gotBody, "b=2") {
		t.Fatalf("form encoding expected, got %q", gotBody)
	}
}

// TestPostJSONBodyPreservesUnicode 确认 JSON 编码不转义非 ASCII
// （ehentai 的 nl / 文件名里会出现中文，转义后上游匹配会失败）。
func TestPostJSONBodyPreservesUnicode(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)
	_, err := h.Call(`(async () => {
		await Network.post(` + jsonQuote(srv.URL) + `, {'Content-Type':'application/json'},
			{ name: "紫の夢" });
		return "ok";
	})()`)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(gotBody, "紫の夢") {
		t.Fatalf("unicode must not be escaped, got %q", gotBody)
	}
}
