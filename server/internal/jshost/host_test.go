package jshost

import (
	"testing"
)

// 这些测试守护与 App 行为一致性的关键点。任何一条失败都意味着
// 真实漫画源会在服务端表现异常。

func newTestHost(t *testing.T, sourceJS string) *Host {
	t.Helper()
	h, err := New(Options{
		SourceKey: "test",
		InitJS:    testInitJS,
		SourceJS:  sourceJS,
		Timeout:   defaultTestTimeout,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	t.Cleanup(h.Close)
	go h.RunLoop()
	return h
}

// testInitJS 是 assets/init.js 的最小替身：只保留被测 API 的 JS 侧封装。
// 之所以不直接用完整 init.js，是为了让单测不依赖 40KB 的资产文件，
// 从而可以在任何环境下运行（集成测试另有覆盖）。
const testInitJS = `
async function fetch(url, options) {
	let method = 'GET', headers = {}, data = null;
	if (options) { method = options.method || method; headers = options.headers || headers; data = options.body || data; }
	let result = await Network.fetchBytes(method, url, headers, data);
	return {
		ok: result.status >= 200 && result.status < 300,
		status: result.status,
		headers: result.headers,
		arrayBuffer: async () => result.body,
		text: async () => Convert.decodeUtf8(result.body),
		json: async () => JSON.parse(Convert.decodeUtf8(result.body)),
	}
}

let Network = {
	async fetchBytes(method, url, headers, data) {
		return sendMessage({method: 'http', http_method: method, bytes: true, url: url, headers: headers, data: data});
	},
	async sendRequest(method, url, headers, data) {
		return sendMessage({method: 'http', http_method: method, url: url, headers: headers, data: data});
	},
	async get(url, headers) { return this.sendRequest('GET', url, headers); },
	async post(url, headers, data) { return this.sendRequest('POST', url, headers, data); },
	getCookies(url) { return sendMessage({method: 'cookie', function: 'get', url: url}); },
	setCookies(url, cookies) { return sendMessage({method: 'cookie', function: 'set', url: url, cookies: cookies}); },
}

let Convert = {
	encodeUtf8: (v) => sendMessage({method:"convert", type:"utf8", value:v, isEncode:true}),
	decodeUtf8: (v) => sendMessage({method:"convert", type:"utf8", value:v, isEncode:false}),
	encodeBase64: (v) => sendMessage({method:"convert", type:"base64", value:v, isEncode:true}),
	decodeBase64: (v) => sendMessage({method:"convert", type:"base64", value:v, isEncode:false}),
	md5: (v) => sendMessage({method:"convert", type:"md5", value:v, isEncode:true}),
	sha256: (v) => sendMessage({method:"convert", type:"sha256", value:v, isEncode:true}),
	hmacString: (k, v, h) => sendMessage({method:"convert", type:"hmac", value:v, key:k, hash:h, isEncode:true, isString:true}),
	hexEncode: (v) => sendMessage({method:"convert", type:"hexEncode", value:v}),
}

function setTimeout(cb, ms) { sendMessage({method:'delay', time: ms}).then(cb); }

class ComicSource {
	key = "";
	loadData(k) { return sendMessage({method:'load_data', key:this.key, data_key:k}); }
	loadSetting(k) { return sendMessage({method:'load_setting', key:this.key, setting_key:k}); }
	saveData(k, v) { return sendMessage({method:'save_data', key:this.key, data_key:k, data:v}); }
}
`

const defaultTestTimeout = 5_000_000_000 // 5s

func TestClassNameDetection(t *testing.T) {
	cases := []struct{ js, want string }{
		{"class Foo extends ComicSource {\n}", "Foo"},
		{"// comment\nclass Bar extends ComicSource{", "Bar"},
		{"\r\nclass Baz extends ComicSource {\r\n}", "Baz"},
	}
	for _, c := range cases {
		got, err := detectClassName(c.js)
		if err != nil {
			t.Fatalf("detect %q: %v", c.js, err)
		}
		if got != c.want {
			t.Fatalf("detect(%q) = %q, want %q", c.js, got, c.want)
		}
	}
	if _, err := detectClassName("class X extends SomethingElse {}"); err == nil {
		t.Fatal("expected error for non-ComicSource class")
	}
}

func TestConvertUTF8AndBase64(t *testing.T) {
	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)

	out, err := h.Call(`(() => {
		const b = Convert.encodeUtf8("你好 world");
		const s = Convert.decodeUtf8(b);
		const b64 = Convert.encodeBase64(Convert.encodeUtf8("abc"));
		const back = Convert.decodeUtf8(Convert.decodeBase64(b64));
		return JSON.stringify({s, b64, back});
	})()`)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := out.String()
	want := `{"s":"你好 world","b64":"YWJj","back":"abc"}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestConvertMD5Hex(t *testing.T) {
	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)
	out, err := h.Call(`(() => {
		const d = Convert.md5(Convert.encodeUtf8("abc"));
		const hex = Convert.hexEncode ? Convert.hexEncode(d) : "no-hex";
		return hex;
	})()`)
	if err != nil {
		t.Fatalf("md5: %v", err)
	}
	// md5("abc") = 900150983cd24fb0d6963f7d28e17f72
	if out.String() != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("md5 mismatch: %s", out.String())
	}
}

func TestConvertHMAC(t *testing.T) {
	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)
	out, err := h.Call(`(() => {
		const key = Convert.encodeUtf8("key");
		const val = Convert.encodeUtf8("The quick brown fox jumps over the lazy dog");
		return Convert.hmacString(key, val, "sha256");
	})()`)
	if err != nil {
		t.Fatalf("hmac: %v", err)
	}
	const want = "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if out.String() != want {
		t.Fatalf("hmac mismatch:\n got  %s\n want %s", out.String(), want)
	}
}

func TestDelayPromise(t *testing.T) {
	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)
	out, err := h.Call(`(async () => {
		const t0 = Date.now();
		await new Promise(r => setTimeout(r, 60));
		return Date.now() - t0;
	})()`)
	if err != nil {
		t.Fatalf("delay: %v", err)
	}
	ms := toInt(out.Export())
	if ms < 50 || ms > 2000 {
		t.Fatalf("setTimeout did not wait properly: %d ms", ms)
	}
}

func TestSourceDataPersistence(t *testing.T) {
	ms := newMemStore()
	h, err := New(Options{
		SourceKey: "test",
		InitJS:    testInitJS,
		SourceJS:  `class T extends ComicSource { key = "test"; }`,
		Store:     ms,
		Timeout:   defaultTestTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	go h.RunLoop()

	// 写数据
	if _, err := h.Call(`(() => {
		__source.saveData("token", "abc123");
		return "ok";
	})()`); err != nil {
		t.Fatalf("saveData: %v", err)
	}
	// 读回来
	out, err := h.Call(`(() => __source.loadData("token"))()`)
	if err != nil {
		t.Fatalf("loadData: %v", err)
	}
	if out.String() != "abc123" {
		t.Fatalf("got %q, want abc123", out.String())
	}
}

// loadSetting 必须返回 App 同步过来的真实设置值。
// 回归背景：服务端此前对该请求一律返回 null，源脚本把
// `${loadSetting('base_url')}/comics/x` 拼成 "null/comics/x"，
// 导致 picacg 等源全部报 invalid url。
func TestLoadSettingReadsSyncedValue(t *testing.T) {
	ms := newMemStore()
	ms.data["picacg"] = map[string]any{
		"settings": map[string]any{
			"base_url":   "https://api.example.com",
			"appChannel": "1",
			"nested":     map[string]any{"deep": "v"},
		},
		"account": []any{"user", "pass"},
	}
	h, err := New(Options{
		SourceKey: "picacg",
		InitJS:    testInitJS,
		SourceJS: `class T extends ComicSource {
			key = "picacg";
			settings = {
				base_url: { title: "API address", type: "input", default: "https://api.example.com" },
				appChannel: { type: "select", default: "3" },
			};
		}`,
		Store:   ms,
		Timeout: defaultTestTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	go h.RunLoop()

	cases := []struct{ expr, want string }{
		{`__source.loadSetting("base_url")`, "https://api.example.com"},
		{`__source.loadSetting("appChannel")`, "1"},
		{`__source.loadSetting("nested.deep")`, "v"},
	}
	for _, c := range cases {
		out, err := h.Call(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := out.String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}

	// 未设置的键必须回落到源脚本声明的 default，
	// 而不是把 undefined 拼进 URL。
	for _, c := range []struct{ key, want string }{
		{"base_url", "https://api.example.com"}, // 库里没存 → 用 default
		{"not_declared", "undefined"},
	} {
		out, err := h.Call(`(() => {
			const v = __source.loadSetting(` + "`" + c.key + "`" + `);
			return v === undefined ? "undefined" : String(v);
		})()`)
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != c.want {
			t.Errorf("loadSetting(%q) = %q, want %q", c.key, out.String(), c.want)
		}
	}
}

func TestPromiseRejectionPropagates(t *testing.T) {	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)
	_, err := h.Call(`(async () => { throw new Error("boom"); })()`)
	if err == nil {
		t.Fatal("expected error from rejected promise")
	}
	if !contains(err.Error(), "boom") {
		t.Fatalf("error lost the message: %v", err)
	}
}

func TestTimeoutEnforced(t *testing.T) {
	h, err := New(Options{
		SourceKey: "test",
		InitJS:    testInitJS,
		SourceJS:  `class T extends ComicSource { key = "test"; }`,
		Timeout:   300_000_000, // 300ms
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	go h.RunLoop()

	_, err = h.Call(`(async () => {
		await new Promise(r => setTimeout(r, 5000));
		return "never";
	})()`)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !contains(err.Error(), "timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------- HTML DOM ----------

func TestHTMLSelector(t *testing.T) {
	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)
	// 注意：这里直接验证 Go 侧选择器引擎
	doc, err := newDocument(`<html><body>
		<div id="main" class="content wide">
			<a href="/a" class="item first">A</a>
			<a href="/b" class="item">B</a>
			<span data-x="1">C</span>
		</div>
	</body></html>`)
	if err != nil {
		t.Fatal(err)
	}

	if n := querySelector(doc.root, "#main"); n == nil {
		t.Fatal("#main not found")
	}
	if n := querySelector(doc.root, ".content.wide"); n == nil {
		t.Fatal(".content.wide not found")
	}
	if n := querySelector(doc.root, "div > a"); n == nil {
		t.Fatal("div > a not found")
	}
	if got := len(querySelectorAll(doc.root, "a.item")); got != 2 {
		t.Fatalf("a.item count = %d, want 2", got)
	}
	if n := querySelector(doc.root, `a[href="/b"]`); n == nil {
		t.Fatal("attribute selector failed")
	}
	if n := querySelector(doc.root, `span[data-x]`); n == nil {
		t.Fatal("bare attribute selector failed")
	}
	if got := textOf(querySelector(doc.root, "#main")); contains(got, "A") == false {
		t.Fatalf("textOf lost content: %q", got)
	}
	// 后代与相邻
	if n := querySelector(doc.root, "div a"); n == nil {
		t.Fatal("descendant selector failed")
	}
	if n := querySelector(doc.root, "a + span"); n == nil {
		t.Fatal("adjacent sibling selector failed")
	}
	_ = h
}

func TestHTMLTextExcludesScript(t *testing.T) {
	doc, err := newDocument(`<div>hello<script>evil()</script><style>.x{}</style>world</div>`)
	if err != nil {
		t.Fatal(err)
	}
	got := textOf(doc.root)
	if contains(got, "evil") || contains(got, ".x") {
		t.Fatalf("script/style leaked into text: %q", got)
	}
	if !contains(got, "hello") || !contains(got, "world") {
		t.Fatalf("text lost: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
