package jshost

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/venera-app/venera-nas/assets"
)

// jsQuote 把 Go 字符串转成 JS 字面量（等价于 source.jsString）。
// 测试里本地实现，避免为测试把包内 helper 导出。
func jsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// newDOMTestHost 造一个带完整 init.js 的 Host。
//
// 必须用真实的 assets.InitJS：精简版 testInitJS 里没有 HtmlDocument，
// 而 ehentai 的 DOM 解析路径正是靠它。
func newDOMTestHost(t *testing.T) *Host {
	t.Helper()
	h, err := New(Options{
		SourceKey: "test",
		InitJS:    assets.InitJS,
		SourceJS:  `class T extends ComicSource { key = "test"; }`,
		Timeout:   30 * 1e9,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	t.Cleanup(h.Close)
	go h.RunLoop()
	return h
}

// TestScriptElementTextIsReadable 锁住 ehentai 全线失败的根因。
//
// 源脚本（ehentai.js loadInfo）这样取图片 token：
//
//	let script = document.querySelectorAll("script")
//	                 .find((e) => e.text.includes("var token"));
//	for (let match of script.text.matchAll(reg)) { ... }
//
// 早期 textOf() 对任何 script 元素都返回空串（它把 script 当成
// "不参与可见文本"的容器），于是：
//   - e.text 恒为空，find() 返回 undefined；
//   - 紧接着 script.text 抛 Cannot read property 'text' of undefined。
//
// 实测影响：107 个 ehentai 任务里 101 个都倒在这一行，
// 而同一份 Cookie 直接请求能拿到 82KB 完整页面且确实含 var token。
func TestScriptElementTextIsReadable(t *testing.T) {
	h := newDOMTestHost(t)

	const page = `<!doctype html><html><head>
<script>var gid = 2643494; var token = "abc123";</script>
</head><body><h1 id="gn">Title</h1></body></html>`

	v, err := h.Call(`(() => {
		const doc = new HtmlDocument(` + jsQuote(page) + `);
		const scripts = doc.querySelectorAll("script");
		const hit = scripts.find((e) => e.text.includes("var token"));
		const out = {
			count: scripts.length,
			firstTextLen: scripts.length ? scripts[0].text.length : -1,
			found: !!hit,
			hasToken: hit ? hit.text.includes("var token") : false,
		};
		doc.dispose();
		return JSON.stringify(out);
	})()`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := v.String()
	t.Logf("result: %s", got)

	if !strings.Contains(got, `"found":true`) {
		t.Fatalf("script containing 'var token' must be found, got %s", got)
	}
	if !strings.Contains(got, `"hasToken":true`) {
		t.Fatalf("script text must be readable, got %s", got)
	}
	if strings.Contains(got, `"firstTextLen":0`) {
		t.Fatalf("script element text must not be empty, got %s", got)
	}
}

// TestBodyTextSkipsScript 确认容器文本仍然不把脚本内容混进去
// （否则页面文本搜索、标题解析会被 JS 源码污染）。
func TestBodyTextSkipsScript(t *testing.T) {
	h := newDOMTestHost(t)

	const page = `<html><body>
<script>var secret = "should_not_appear";</script>
<p>Visible Text</p>
</body></html>`

	v, err := h.Call(`(() => {
		const doc = new HtmlDocument(` + jsQuote(page) + `);
		const b = doc.querySelector("body");
		const txt = b ? b.text : "";
		doc.dispose();
		return txt;
	})()`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := v.String()
	if !strings.Contains(got, "Visible Text") {
		t.Fatalf("body text must contain visible text, got %q", got)
	}
	if strings.Contains(got, "should_not_appear") {
		t.Fatalf("body text must not include script content, got %q", got)
	}
}

// TestRealEhentaiLoadInfoParses 用真实 ehentai 页面验证 loadInfo 的关键
// 解析步骤能走通（Cookie + 代理可用时才跑）。
//
// 它与 TestEhentaiGalleryPageHasToken 互补：后者验证网络层拿到的页面
// 含 var token，这里验证 DOM 层能把它取出来。
func TestRealEhentaiLoadInfoParses(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	h, ok := newLiveEhentaiHost(t)
	if !ok {
		return
	}
	defer h.Close()
	go h.RunLoop()

	const url = "https://exhentai.org/g/2643494/a63e95900d/"
	v, err := h.Call(`(async () => {
		const res = await Network.get(` + jsQuote(url) + `);
		if (res.status !== 200) return JSON.stringify({error: "status " + res.status});
		const doc = new HtmlDocument(res.body);
		const scripts = doc.querySelectorAll("script");
		const hit = scripts.find((e) => e.text.includes("var token"));
		const reg = RegExp("var\\s+(\\w+)\\s*=\\s*(.*?);", "g");
		let vars = {};
		if (hit) {
			for (let m of hit.text.matchAll(reg)) vars[m[1]] = m[2];
		}
		const h1 = doc.querySelector("h1#gn");
		const title = h1 ? h1.text : null;
		doc.dispose();
		return JSON.stringify({
			hasHit: !!hit, title: title,
			varCount: Object.keys(vars).length,
			gid: vars.gid || null,
		});
	})()`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := v.String()
	t.Logf("result: %s", got)

	if strings.Contains(got, `"error"`) {
		t.Skipf("upstream unavailable right now: %s", got)
	}
	if !strings.Contains(got, `"hasHit":true`) {
		t.Fatalf("loadInfo would crash: no script with 'var token'. got %s", got)
	}
	if strings.Contains(got, `"varCount":0`) {
		t.Fatalf("token regex extracted nothing. got %s", got)
	}
}
