package source

import (
	"strings"
	"testing"
)

// TestEhentaiShowpageApi 直接打 ehentai 的 showpage API，
// 确认服务端能否拿到图片地址。
//
// 这一层是 ehentai 下载失败的真正卡点：
//   - loadEp 返回页索引（已验证）；
//   - onImageLoad → getImageFromApi 需要 showkey + imgkey + nl；
//   - 返回体里 i6 字段形如 "nl('xxxx')"，用正则取出下一个 nl。
//
// 如果 showpage 返回的 i6 结构变化，源脚本第 1007 行的
// `reg[1]` 就会炸（我们观测到的正是这个）。
func TestEhentaiShowpageApi(t *testing.T) {
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
		const key = await s.comic.getKey(first.urls[0]);
		const out = { showkey: key.showkey || null, mpvkey: key.mpvkey || null };

		// 模拟源脚本的 showpage 分支
		const thumbUrl = first.urls[0];
		const imgkey = thumbUrl.split("/")[4];
		const res = await Network.post(s.apiUrl, {'Content-Type':'application/json'}, {
			gid: s.parseUrl(`+jsQuoteSrc(cid)+`).id,
			imgkey: imgkey,
			method: "showpage",
			page: 1,
			showkey: key.showkey,
			nl: null,
		});
		out.status = res.status;
		let json = null;
		try { json = JSON.parse(res.body); } catch(e) { out.parseError = String(e); }
		if (json) {
			out.hasI3 = typeof json.i3 === "string";
			out.hasI6 = typeof json.i6 === "string";
			out.i6 = json.i6 || null;
			out.i6Match = json.i6 ? RegExp("nl\\('(.+?)'\\)").exec(json.i6) !== null : false;
			out.i3Head = json.i3 ? json.i3.slice(0, 120) : null;
			// 记录完整的响应形状，便于看清上游改了什么
			out.keys = Object.keys(json);
			out.raw = res.body.slice(0, 600);
		}
		return JSON.stringify(out);
	})()`)
	if err != nil {
		t.Skipf("upstream unavailable: %v", err)
	}
	got := v.String()
	t.Logf("showpage result: %s", got)

	if strings.Contains(got, `"showkey":null`) && strings.Contains(got, `"mpvkey":null`) {
		t.Skip("getKey produced no key; upstream shape changed")
	}
	if strings.Contains(got, `"hasI6":true`) && !strings.Contains(got, `"i6Match":true`) {
		t.Errorf("i6 present but nl() regex does not match -> source script line 1007 will crash")
	}
}
