package jshost

import (
	"bytes"
	"image"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	// WebP 是 jm 等 CDN 的主力格式（URL 直接以 .webp 结尾），
	// 标准库不认识。x/image/webp 提供解码器，注册后 image.Decode 即可。
	_ "golang.org/x/image/webp"
)

// bytesReader 是一个小包装，避免在多个文件重复 import bytes。
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// decodeAnyImage 解码常见图片格式（jpeg/png/gif/webp）。
func decodeAnyImage(b []byte) (image.Image, string, error) {
	return image.Decode(bytes.NewReader(b))
}
