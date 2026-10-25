package jshost

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// bytesReader 是一个小包装，避免在多个文件重复 import bytes。
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// decodeAnyImage 解码常见图片格式（jpeg/png/gif）。
// WebP 不在 Go 标准库内，遇到时返回明确错误。
func decodeAnyImage(b []byte) (image.Image, string, error) {
	return image.Decode(bytes.NewReader(b))
}
