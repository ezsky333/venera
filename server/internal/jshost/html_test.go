package jshost

import (
	"strings"
	"testing"

	"github.com/venera-app/venera-nas/assets"
	"github.com/venera-app/venera-nas/internal/model"
)

// domTestStore 是最小的 SourceDataStore，供 DOM 相关测试使用。
type domTestStore struct{}

func (m *domTestStore) GetSourceData(string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (m *domTestStore) MergeSourceData(string, map[string]any) error { return nil }
func (m *domTestStore) GetCookies(string) ([]model.CookieRecord, error) {
	return nil, nil
}
func (m *domTestStore) PutCookies(string, []model.CookieRecord) error { return nil }

// newDOMHost 用真实 assets/init.js 构造 Host，因为 DOM 封装（HtmlDocument /
// HtmlElement）就声明在那里——用替身测不到真实的参数布局。
func newDOMHost(t *testing.T) *Host {
	t.Helper()
	h, err := New(Options{
		SourceKey: "_domtest",
		InitJS:    assets.InitJS,
		SourceJS:  "class _DomTest extends ComicSource { key = '_domtest'; }",
		Store:     &domTestStore{},
		Timeout:   defaultTestTimeout,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	go h.RunLoop()
	return h
}

// TestDocumentQuerySelectorStartsFromRoot 锁住这个真实 bug：
//
//	HtmlDocument.querySelector 只传 {key: docKey}，没有 doc 字段。
//	旧实现在 msg["doc"] 缺失时拿到 0，绑定到"文档 0"，
//	并把 docKey 当成元素 id，查询范围被缩小到某棵子树，
//	`#comicName` 这类跨子树的选择器永远查不到。
//
// wnacg 因此整段 loadInfo 失败（"Cannot read property 'text' of undefined"）。
func TestDocumentQuerySelectorStartsFromRoot(t *testing.T) {
	h := newDOMHost(t)
	defer h.Close()

	html := `<html><body>
		<div id="comicName">TITLE-HERE</div>
		<div class="sub_r"><span class="date">223P</span></div>
	</body></html>`

	// 关键顺序：先查 .sub_r（它会被 intern 成 id 0），再查 #comicName。
	// 如果实现把 docKey 当元素 id，就会在 .sub_r 子树里找 #comicName 而失败。
	expr := `(() => {
		const d = new HtmlDocument(` + domJSQuote(html) + `);
		const first = d.querySelector("div.sub_r");
		const title = d.querySelector("#comicName");
		const out = {
			firstOk: first !== null,
			titleText: title === null ? null : title.text,
		};
		d.dispose();
		return JSON.stringify(out);
	})()`

	v, err := h.Call(expr)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := v.String()
	if !strings.Contains(got, `"titleText":"TITLE-HERE"`) {
		t.Fatalf("document-level querySelector must search from root, got %s", got)
	}
}

// TestElementTextIsNotWholeDocument 锁住 key=0 的坐标歧义：
//
//	第一个文档的 key 就是 0。若用 `doc == 0` 判断"文档级调用"，
//	HtmlElement{key:0, doc:0} 会被误判为文档级，`.text` 返回整页文本。
// 症状：漫画标题变成整页所有文字（wnacg 实际出现过）。
func TestElementTextIsNotWholeDocument(t *testing.T) {
	h := newDOMHost(t)
	defer h.Close()

	html := `<html><body><div id="comicName">INNER</div><div>OUTER-NOISE</div></body></html>`
	expr := `(() => {
		const d = new HtmlDocument(` + domJSQuote(html) + `);
		const el = d.querySelector("#comicName");
		const text = el.text;
		d.dispose();
		return text;
	})()`

	v, err := h.Call(expr)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := v.String(); got != "INNER" {
		t.Fatalf("element .text must be its own text, got %q", got)
	}
}

// TestDocumentQuerySelectorAllReturnsArray 锁住 wnacg 的第二个真实 bug：
//
//	文档级 querySelectorAll 若返回 null，JS 侧 `ks.map(...)` 会抛
//	"Cannot read property 'map' of undefined or null"，
//	表现为"域名列表页明明抓到了，却一个域名都解析不出来"。
func TestDocumentQuerySelectorAllReturnsArray(t *testing.T) {
	h := newDOMHost(t)
	defer h.Close()

	html := `<html><body>
		<a href="https://a.example/">a</a>
		<a href="https://b.example/">b</a>
	</body></html>`
	expr := `(() => {
		const d = new HtmlDocument(` + domJSQuote(html) + `);
		const links = d.querySelectorAll("a[href]");
		const hrefs = links.map(e => e.attributes["href"]);
		d.dispose();
		return JSON.stringify(hrefs);
	})()`

	v, err := h.Call(expr)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := v.String()
	if !strings.Contains(got, "a.example") || !strings.Contains(got, "b.example") {
		t.Fatalf("document-level querySelectorAll must find both links, got %s", got)
	}
}

// TestSelectorWithSpaceInsideAttribute 锁住解析器 bug：
//
//	`[title="a b"]` 里的空格不是后代组合符。旧 parseChain 会在空格处断开，
//	后半截被当成 tag 名，整个选择器静默失效（返回 null，不报错）。
func TestSelectorWithSpaceInsideAttribute(t *testing.T) {
	h := newDOMHost(t)
	defer h.Close()

	html := `<html><body><div title="hello world">HIT</div><div title="other">MISS</div></body></html>`
	expr := `(() => {
		const d = new HtmlDocument(` + domJSQuote(html) + `);
		const el = d.querySelector('div[title="hello world"]');
		const text = el === null ? null : el.text;
		d.dispose();
		return text === null ? "NULL" : text;
	})()`

	v, err := h.Call(expr)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := v.String(); got != "HIT" {
		t.Fatalf(`selector with space inside [attr="..."] must match, got %q`, got)
	}
}

// domJSQuote 把 Go 字符串包成 JS 字符串字面量。
func domJSQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
