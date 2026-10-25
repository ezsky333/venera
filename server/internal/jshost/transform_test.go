package jshost

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/venera-app/venera-nas/assets"
)

// mkPNG 生成一张纯色 PNG 用于变换测试。
func mkPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestOnResponseXor 复现 comic_walker 类源的做法：
// 对响应字节做 XOR 后再交给图片解码器。
func TestOnResponseXor(t *testing.T) {
	// 源里的典型写法（c.onResponse.toString() 的形态）
	const script = `function (buffer) {
		var key = [1, 2, 3, 4];
		var view = new Uint8Array(buffer);
		for (var i = 0; i < view.length; i++) {
			view[i] ^= key[i % key.length];
		}
		return buffer;
	}`

	// 造一份"加密"数据：把 PNG 明文异或一次
	plain := mkPNG(t, 8, 8, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	key := []byte{1, 2, 3, 4}
	cipher := make([]byte, len(plain))
	for i := range plain {
		cipher[i] = plain[i] ^ key[i%len(key)]
	}

	// 用完整 init.js，确保真实环境下的语义
	out, err := TransformOnResponse(assets.InitJS, script, cipher)
	if err != nil {
		t.Fatalf("TransformOnResponse: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("XOR transform mismatch: got %d bytes, want %d", len(out), len(plain))
	}
	// 且结果应能被解码成图片
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("transformed data is not a valid image: %v", err)
	}
}

// TestOnResponseIdentity 源返回未改动的 buffer，不应破坏数据。
func TestOnResponseIdentity(t *testing.T) {
	const script = `function (buffer) { return buffer; }`
	plain := mkPNG(t, 4, 4, color.RGBA{A: 255})
	out, err := TransformOnResponse(assets.InitJS, script, plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatal("identity transform changed the data")
	}
}

// TestOnResponseArrowFunction 箭头函数形态（部分源用 toString() 得到）。
func TestOnResponseArrowFunction(t *testing.T) {
	const script = `(buffer) => {
		var view = new Uint8Array(buffer);
		for (var i = 0; i < view.length; i++) view[i] ^= 0xFF;
		return buffer;
	}`
	plain := mkPNG(t, 4, 4, color.RGBA{A: 255})
	cipher := make([]byte, len(plain))
	for i := range plain {
		cipher[i] = plain[i] ^ 0xFF
	}
	out, err := TransformOnResponse(assets.InitJS, script, cipher)
	if err != nil {
		t.Fatalf("arrow fn: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatal("arrow function transform mismatch")
	}
}

// TestModifyImageVerticalFlip 复现 jm.js 的切片重排：
// 把图片切成 N 段后逆序拼接。这里用 2 段翻转验证等价效果。
func TestModifyImageVerticalFlip(t *testing.T) {
	// 上半红、下半蓝，便于验证翻转结果
	const w, h = 10, 20
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := color.RGBA{R: 255, A: 255}
		if y >= h/2 {
			c = color.RGBA{B: 255, A: 255}
		}
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	data := buf.Bytes()

	// jm 的写法：切成 num 段，从后往前填
	const script = `let modifyImage = (image) => {
		const num = 2;
		let blockSize = Math.floor(image.height / num);
		let remainder = image.height % num;
		let blocks = [];
		for (let i = 0; i < num; i++) {
			let start = i * blockSize;
			let end = start + blockSize + (i !== num - 1 ? 0 : remainder);
			blocks.push({start: start, end: end});
		}
		let res = Image.empty(image.width, image.height);
		let y = 0;
		for (let i = blocks.length - 1; i >= 0; i--) {
			let block = blocks[i];
			let currentHeight = block.end - block.start;
			res.fillImageRangeAt(0, y, image, 0, block.start, image.width, currentHeight);
			y += currentHeight;
		}
		return res;
	}`

	out, err := TransformModifyImage(assets.InitJS, script, data)
	if err != nil {
		t.Fatalf("TransformModifyImage: %v", err)
	}

	got, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.Bounds().Dx() != w || got.Bounds().Dy() != h {
		t.Fatalf("size changed: %v", got.Bounds())
	}

	// 翻转后：上部应为蓝色（原下身）
	r, g, b, _ := got.At(5, 2).RGBA()
	if !(b > r && b > g) {
		t.Fatalf("top should be blue after flip, got r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
	// 下部应为红色
	r2, g2, b2, _ := got.At(5, h-3).RGBA()
	if !(r2 > g2 && r2 > b2) {
		t.Fatalf("bottom should be red after flip, got r=%d g=%d b=%d", r2>>8, g2>>8, b2>>8)
	}
}

// TestImageOps 直接验证 Go 侧 Image API 与 App 语义一致。
func TestImageOps(t *testing.T) {
	h := newTestHost(t, `class T extends ComicSource { key = "test"; }`)

	// 构造一个 4x4 的图像并注册
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 60), A: 255})
		}
	}
	h.helpers.imgMu.Lock()
	h.helpers.imgSeq++
	id := h.helpers.imgSeq
	h.helpers.imgs.Store(id, src)
	h.helpers.imgMu.Unlock()

	// copyRange
	v, err := h.Call(`(() => { const r = sendMessage({method:"image", function:"copyRange", key:` +
		itoa(id) + `, x:1, y:1, width:2, height:2}); return r; })()`)
	if err != nil {
		t.Fatalf("copyRange: %v", err)
	}
	newID := toInt(v.Export())
	sub, ok := h.helpers.imgs.Load(newID)
	if !ok {
		t.Fatal("copyRange produced no image")
	}
	sb := sub.(image.Image).Bounds()
	if sb.Dx() != 2 || sb.Dy() != 2 {
		t.Fatalf("copyRange size = %dx%d, want 2x2", sb.Dx(), sb.Dy())
	}

	// getWidth / getHeight
	v, err = h.Call(`(() => sendMessage({method:"image", function:"getWidth", key:` +
		itoa(id) + `}))()`)
	if err != nil {
		t.Fatal(err)
	}
	if got := toInt(v.Export()); got != 4 {
		t.Fatalf("getWidth = %d, want 4", got)
	}

	// 越界 copyRange 应返回 null 而不是 panic
	v, err = h.Call(`(() => sendMessage({method:"image", function:"copyRange", key:` +
		itoa(id) + `, x:3, y:3, width:5, height:5}))()`)
	if err != nil {
		t.Fatalf("out-of-range should not error: %v", err)
	}
	if !gojaIsNull(v) {
		t.Fatalf("out-of-range copyRange should return null, got %v", v)
	}
}

func gojaIsNull(v interface{ Export() any }) bool {
	return v == nil || v.Export() == nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
