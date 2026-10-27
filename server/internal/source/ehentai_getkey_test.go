package source

import (
	"strings"
	"testing"
	"time"
)

// TestEhentaiGetKeyBranch 诊断 ehentai 的 getKey 走哪个分支。
//
// getKey 有两条路径：
//   - showkey（老式单页图集）
//   - mpvkey + imagelist（新式多页 / H@H 分发）
//
// 源脚本在 showkey 分支里写的是 `match[1]` 且没有 null 保护：
// 只要页面里有 "showkey" 字样、但格式不是 `showkey="..."`，
// 就会抛 "Cannot read property '1' of undefined"。
//
// 这个测试只做观测，不判定失败，方便确认真实画廊当前形态。
func TestEhentaiGetKeyBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	m, ok := newLiveEhentaiManager(t)
	if !ok {
		return
	}
	const cid = "https://exhentai.org/g/2643494/a63e95900d/"

	v, err := m.withHostForTest("ehentai", `(async () => {
		const s = __source;
		const first = await s.comic.loadThumbnails(`+jsQuoteSrc(cid)+`);
		const page = await Network.get(first.urls[0], {'cache-time':'long'});
		const doc = new HtmlDocument(page.body);
		const scripts = doc.querySelectorAll("script");
		const withShow = scripts.filter((e) => e.text.includes("showkey"));
		const withMpv = scripts.filter((e) => e.text.includes("mpvkey"));
		const showMatch = withShow.length
			? RegExp('showkey="(.*?)"', "g").exec(withShow[0].text) : null;
		const out = {
			status: page.status,
			scriptCount: scripts.length,
			showkeyScripts: withShow.length,
			mpvkeyScripts: withMpv.length,
			showkeyMatched: !!showMatch,
			firstThumb: first.urls[0],
			thumbCount: first.thumbnails.length,
		};
		doc.dispose();
		return JSON.stringify(out);
	})()`)
	if err != nil {
		t.Skipf("diagnostic unavailable: %v", err)
	}
	t.Logf("getKey diagnostics: %s", v.String())

	if !strings.Contains(v.String(), `"showkeyMatched":true`) &&
		strings.Contains(v.String(), `"showkeyScripts":1`) {
		t.Logf("CONFIRMED: page has 'showkey' text but regex does not match -> source script will crash at match[1]")
	}
	_ = time.Second
}

func jsQuoteSrc(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
