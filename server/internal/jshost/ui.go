package jshost

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"sync"

	"github.com/dop251/goja"
)

// jsHelpers 持有与 JS 运行时关联的辅助状态：
//   - docs:   HTML 文档表（整数 key -> Document）
//   - imgs:   Image 对象表（整数 key -> image.Image），用于 modifyImage
type jsHelpers struct {
	docs   sync.Map
	imgs   sync.Map
	imgSeq int64
	imgMu  sync.Mutex
}

// jsUI 实现 UI.showMessage / showDialog / showInputDialog 等交互接口。
//
// 无人值守场景下这些交互没有意义。App 的语义是"弹窗并等待用户输入"，
// 服务端采用以下降级策略，保证流程不会永久阻塞：
//   - showMessage / launchUrl: 记录日志后返回
//   - showDialog:             立即 resolve（不执行任何 action）
//   - showInputDialog:        返回 null（源会走"用户取消"分支）
//   - showSelectDialog:       返回 0（选第一项，比 null 更利于自动化继续）
//   - showLoading:            返回一个可用的 loading id
//   - cancelLoading:          no-op
func (h *Host) jsUI(msg map[string]any) goja.Value {
	fn, _ := msg["function"].(string)
	switch fn {
	case "showMessage":
		h.opts.Logger("info", "UI", fmt.Sprint(msg["message"]))
		return goja.Undefined()

	case "showInputDialog":
		h.opts.Logger("warning", "UI", "showInputDialog unsupported in headless mode; returning null")
		return goja.Null()

	case "showSelectDialog":
		h.opts.Logger("warning", "UI", "showSelectDialog unsupported in headless mode; returning 0")
		return h.vm.ToValue(0)

	case "showLoading":
		return h.vm.ToValue(1)

	case "cancelLoading":
		return goja.Undefined()

	case "showDialog":
		h.opts.Logger("info", "UI", fmt.Sprintf("dialog suppressed: %v", msg["title"]))
		p, resolve, _ := h.vm.NewPromise()
		h.post(func() { resolve(goja.Undefined()) })
		return h.vm.ToValue(p)

	case "launchUrl":
		h.opts.Logger("info", "UI", fmt.Sprintf("launchUrl suppressed: %v", msg["url"]))
		return goja.Undefined()
	}
	return goja.Undefined()
}

// jsCompute 执行源的 compute(funcStr, ...args)。
// App 会把函数投递到独立 isolate 池；这里在同一个 VM 内同步执行，
// 语义等价（源只用它做纯计算，不依赖线程隔离）。
func (h *Host) jsCompute(fnStr string, args []any) goja.Value {
	if fnStr == "" {
		return goja.Null()
	}
	fnVal, err := h.vm.RunString("(" + fnStr + ")")
	if err != nil {
		h.opts.Logger("warning", "compute", err.Error())
		return goja.Null()
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return goja.Null()
	}
	callArgs := make([]goja.Value, 0, len(args))
	for _, a := range args {
		callArgs = append(callArgs, h.vm.ToValue(a))
	}
	out, err := fn(goja.Undefined(), callArgs...)
	if err != nil {
		h.opts.Logger("warning", "compute", err.Error())
		return goja.Null()
	}
	return out
}

// jsImage 实现 modifyImage 脚本使用的 Image API。
//
// App 侧用 lodepng + Flutter 解码图片，并通过 JsEngine（lib/utils/image.dart）
// 暴露 copyRange / fillImageAt / fillImageRangeAt / copyAndRotate90 等操作。
// 这里用 Go 的 image 包实现同样的语义，且更快、内存更可控。
//
// modifyImage 目前仅 jm.js 使用（切分长图重排）。
func (h *Host) jsImage(msg map[string]any) goja.Value {
	vm := h.vm
	fn, _ := msg["function"].(string)
	key := toInt(msg["key"])

	getImg := func(id int64) image.Image {
		v, ok := h.helpers.imgs.Load(id)
		if !ok {
			return nil
		}
		img, _ := v.(image.Image)
		return img
	}

	putImg := func(img image.Image) goja.Value {
		h.helpers.imgMu.Lock()
		h.helpers.imgSeq++
		id := h.helpers.imgSeq
		h.helpers.imgMu.Unlock()
		h.helpers.imgs.Store(id, img)
		return vm.ToValue(id)
	}

	switch fn {
	case "emptyImage":
		w := int(toInt(msg["width"]))
		ht := int(toInt(msg["height"]))
		if w <= 0 || ht <= 0 || w*ht > 200_000_000 {
			return goja.Null()
		}
		return putImg(image.NewRGBA(image.Rect(0, 0, w, ht)))

	case "copyRange":
		src := getImg(key)
		if src == nil {
			return goja.Null()
		}
		x, y := int(toInt(msg["x"])), int(toInt(msg["y"]))
		w, ht := int(toInt(msg["width"])), int(toInt(msg["height"]))
		b := src.Bounds()
		if x < 0 || y < 0 || w <= 0 || ht <= 0 || x+w > b.Dx() || y+ht > b.Dy() {
			return goja.Null()
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, ht))
		draw.Draw(dst, dst.Bounds(), src, b.Min.Add(image.Pt(x, y)), draw.Src)
		return putImg(dst)

	case "copyAndRotate90":
		src := getImg(key)
		if src == nil {
			return goja.Null()
		}
		return putImg(rotate90(src))

	case "fillImageAt":
		dst := getImg(key)
		if dst == nil {
			return goja.Null()
		}
		src := getImg(toInt(msg["image"]))
		if src == nil {
			return goja.Null()
		}
		x, y := int(toInt(msg["x"])), int(toInt(msg["y"]))
		into, ok := dst.(draw.Image)
		if !ok {
			return goja.Null()
		}
		draw.Draw(into, image.Rect(x, y, x+src.Bounds().Dx(), y+src.Bounds().Dy()),
			src, src.Bounds().Min, draw.Src)
		return goja.Undefined()

	case "fillImageRangeAt":
		dst := getImg(key)
		if dst == nil {
			return goja.Null()
		}
		src := getImg(toInt(msg["image"]))
		if src == nil {
			return goja.Null()
		}
		x, y := int(toInt(msg["x"])), int(toInt(msg["y"]))
		sx, sy := int(toInt(msg["srcX"])), int(toInt(msg["srcY"]))
		w, ht := int(toInt(msg["width"])), int(toInt(msg["height"]))
		sb := src.Bounds()
		db := dst.Bounds()
		if x < 0 || y < 0 || sx < 0 || sy < 0 || w <= 0 || ht <= 0 {
			return goja.Null()
		}
		if sx+w > sb.Dx() || sy+ht > sb.Dy() || x+w > db.Dx() || y+ht > db.Dy() {
			return goja.Null()
		}
		into, ok := dst.(draw.Image)
		if !ok {
			return goja.Null()
		}
		region := image.NewRGBA(image.Rect(0, 0, w, ht))
		draw.Draw(region, region.Bounds(), src, sb.Min.Add(image.Pt(sx, sy)), draw.Src)
		draw.Draw(into, image.Rect(x, y, x+w, y+ht), region, image.Point{}, draw.Src)
		return goja.Undefined()

	case "getWidth":
		img := getImg(key)
		if img == nil {
			return goja.Null()
		}
		return vm.ToValue(img.Bounds().Dx())

	case "getHeight":
		img := getImg(key)
		if img == nil {
			return goja.Null()
		}
		return vm.ToValue(img.Bounds().Dy())
	}
	return goja.Null()
}

func rotate90(src image.Image) image.Image {
	b := src.Bounds()
	w, ht := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, ht, w))
	for y := 0; y < ht; y++ {
		for x := 0; x < w; x++ {
			dst.Set(ht-1-y, x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// encodePNG 把 image.Image 编码成 PNG（与 App 的 modifyImage 输出格式一致）。
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
