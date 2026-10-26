package jobs

import "testing"

// TestIsRetriableError 锁住"哪些失败值得重试"的判定。
//
// 这张表是运维经验的固化：判错两个方向都有代价——
//   - 把临时故障判成永久：几百个任务停在 failed，永远不再尝试；
//   - 把永久故障判成临时：worker 被"注定失败"的任务反复占住。
func TestIsRetriableError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
		note string
	}{
		// ---- 临时故障：值得重试 ----
		{`Get "https://x/y": EOF`, true, "连接被上游掐断"},
		{`Get "https://x/y": i/o timeout`, true, "超时"},
		{`connection reset by peer`, true, "RST"},
		{`Invalid status code: 429`, true, "限流"},
		{`Invalid status code: 503`, true, "上游维护"},
		{`no such host`, true, "DNS 抖动"},
		// 上游返回 HTML 错误页：源里的 JSON.parse 会报这个
		{`SyntaxError: invalid character '<' looking for beginning of value`, true,
			"上游返回 HTML 拦截图/错误页"},
		{`Unexpected token < in JSON at position 0`, true, "同上（QuickJS 措辞）"},

		// ---- 确定性故障：重试无意义 ----
		{`invalid url: null/comics/123`, false, "源配置缺失（loadSetting 未生效）"},
		{`Custom domain is not set`, false, "wnacg 自定义域名未填"},
		{`Selected domain is unavailable`, false, "域名列表还没刷新"},
		{`Setting not found: apiDomain`, false, "设置键不存在"},
		{`Invalid status code: 302`, false, "exhentai 需要登录 Cookie"},
		{`Invalid Status Code 302`, false, "同上（源的另一种措辞）"},
		{`Invalid status code: 404`, false, "资源确实不存在"},
		{`stopped after 10 redirects`, false, "重定向死循环（登录墙）"},
		{`You may not have permission to access this page.`, false, "权限不足"},
		{`invalid login`, false, "账号密码错误"},
	}

	for _, c := range cases {
		if got := isRetriableError(c.msg); got != c.want {
			t.Errorf("isRetriableError(%q) = %v, want %v (%s)",
				c.msg, got, c.want, c.note)
		}
	}
}

// TestIsRetriableErrorPrefersPermanent 确认"同时含两类关键字"时按永久处理。
//
// 场景：exhentai 的登录墙错误里同时带着 "EOF"（请求过程中的传输错误），
// 若按临时处理就会无限重试一个永远不可能成功的任务。
func TestIsRetriableErrorPrefersPermanent(t *testing.T) {
	msg := `load comic info: Get "https://exhentai.org/g/1/": EOF; ` +
		`Invalid status code: 302`
	if isRetriableError(msg) {
		t.Fatalf("permanent condition must win over transient keyword: %q", msg)
	}
}
