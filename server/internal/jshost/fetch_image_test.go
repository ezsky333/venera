package jshost

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestImageFetchFollowsCrossHostRedirect 锁住这个真实事故：
//
//	picacg 的封面 URL（storage1.picacomic.com）会 301 跳到
//	storage-b.picacomic.com。API 客户端的 CheckRedirect 策略把跨 host
//	重定向拦下（那是给源脚本"识别登录失效"用的），图片抓取复用同一个
//	client，结果全库一万多张内页下载成功、封面只落盘 1 张。
//
// 图片抓取必须使用跟随重定向的 imageClient。
func TestImageFetchFollowsCrossHostRedirect(t *testing.T) {
	// CDN 目的地（另一个 host）
	var cdn *httptest.Server
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3, 4})
	}))
	defer cdn.Close()

	// 源站：301 跳到不同 host 的 CDN
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/real.jpg", http.StatusMovedPermanently)
	}))
	defer origin.Close()

	data, err := FetchImage(nil, origin.URL+"/cover.jpg", nil)
	if err != nil {
		t.Fatalf("image fetch must follow cross-host redirect: %v", err)
	}
	// 命中 CDN 才能拿到真实字节（JPEG 魔数开头）
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatalf("expected CDN bytes (jpeg magic), got %d bytes: %v", len(data), data[:min(8, len(data))])
	}
}

// TestImageFetchKeepsRefererOnRequest 确认默认 Referer 仍然附带：
// 很多图床校验 Referer，丢了它 403。
func TestImageFetchKeepsRefererOnRequest(t *testing.T) {
	var gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))
	defer srv.Close()

	if _, err := FetchImage(nil, srv.URL+"/a.jpg", nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	want := srv.URL + "/"
	if gotReferer != want {
		t.Fatalf("Referer = %q, want %q", gotReferer, want)
	}
}

// TestImageFetchRejectsNon2xx 确认真正的失败（404 等）仍然报错，
// 不因放宽重定向而吞掉错误状态。
func TestImageFetchRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := FetchImage(nil, srv.URL+"/gone.jpg", nil)
	if err == nil {
		t.Fatal("404 must return an error")
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("want status 404 error, got %v", err)
	}
}

// TestAPIKeepsRedirectPolicyForJS 确认 API 客户端（DefaultClient）的
// 跨域拦截**没有**被顺手放宽——那是源脚本判断登录失效的依据。
func TestAPIKeepsRedirectPolicyForJS(t *testing.T) {
	var target *httptest.Server
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	}))
	defer origin.Close()

	client := DefaultClient()
	resp, err := client.Get(origin.URL + "/api")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	// 302 必须原样返回，而不是跟进到 target
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("API client must stop at cross-host 3xx, got status %d (fmt: %v)", resp.StatusCode, fmt.Sprint(resp.StatusCode))
	}
}
