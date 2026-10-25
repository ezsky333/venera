package api

import (
	"net/url"
)

// pathEscape / pathUnescape 是 url.PathEscape 的薄包装，
// 单独成函数便于替换为更严格的实现（例如兼容某些 NAS 客户端的编码差异）。
func pathEscape(s string) string   { return url.PathEscape(s) }
func pathUnescape(s string) (string, error) {
	v, err := url.PathUnescape(s)
	return v, err
}
