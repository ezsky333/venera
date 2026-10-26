package source

import (
	"strings"
	"testing"
	"time"

	"github.com/venera-app/venera-nas/assets"
)

// TestImageConfigForUsesComicOnImageLoad 锁住这个真实 bug：
//
//	jm 等源把图片加载配置挂在 comic.onImageLoad（App 的
//	parser.dart 也是调 comic.onImageLoad(url, comicId, ep)），
//	早期服务端误调 s.getImageLoadingConfig（不存在），永远拿到
//	空配置：jm 的切片重排 modifyImage 丢失，长图按打乱顺序原样
//	落盘，阅读时满屏错位横条；图床请求头也丢了。
func TestImageConfigForUsesComicOnImageLoad(t *testing.T) {
	js := `
class _ImgCfg extends ComicSource {
    key = "_imgcfg"
    name = "spy"
    version = "1.0.0"
    comic = {
        onImageLoad: (url, comicId, epId) => {
            return {
                headers: {"X-Test": "hdr-" + comicId},
                modifyImage: ` + "`" + `
let modifyImage = (image) => {
    const num = 4;
    let res = Image.empty(image.width, image.height);
    return res;
}
` + "`" + `,
            }
        },
    }
}
`
	m := newInitTestManager(t, "_imgcfg", js, &initTestStore{})

	cfg, err := m.ImageConfigFor("_imgcfg", "https://example.com/img/abc.jpg", "cid123", "456")
	if err != nil {
		t.Fatalf("ImageConfigFor: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if cfg.Headers["X-Test"] != "hdr-cid123" {
		t.Fatalf("headers not extracted from comic.onImageLoad: %v", cfg.Headers)
	}
	if !strings.Contains(cfg.ModifyImageJS, "modifyImage") {
		t.Fatalf("modifyImage script not extracted: %q", cfg.ModifyImageJS)
	}
}

// TestImageConfigForJmLikeScramble 用与 jm.js 相同的结构验证
// modifyImage 模板串被原样带回（模板里带 ${num} 占位由源端先求值）。
func TestImageConfigForJmLikeScramble(t *testing.T) {
	js := `
class _JmLike extends ComicSource {
    key = "_jmlike"
    name = "jm-like"
    version = "1.0.0"
    comic = {
        onImageLoad: (url, comicId, epId) => {
            const num = 6;
            return {
                headers: {"Cookie": "AVS=abc"},
                modifyImage: url.endsWith(".gif") ? null : ` + "`" + `
let modifyImage = (image) => {
    const num = ${num};
    let blockSize = Math.floor(image.height / num);
    let remainder = image.height % num;
    let blocks = [];
    for(let i = 0; i < num; i++) {
        let start = i * blockSize;
        let end = start + blockSize + (i !== num - 1 ? 0 : remainder);
        blocks.push({start: start, end: end});
    }
    let res = Image.empty(image.width, image.height);
    let y = 0;
    for(let i = blocks.length - 1; i >= 0; i--) {
        let block = blocks[i];
        let currentHeight = block.end - block.start;
        res.fillImageRangeAt(0, y, image, 0, block.start, image.width, currentHeight);
        y += currentHeight;
    }
    return res;
}
` + "`" + `,
            }
        },
    }
}
`
	m := newInitTestManager(t, "_jmlike", js, &initTestStore{})

	cfg, err := m.ImageConfigFor("_jmlike", "https://cdn/xyz.jpg", "1", "2")
	if err != nil {
		t.Fatalf("ImageConfigFor: %v", err)
	}
	if cfg.Headers["Cookie"] != "AVS=abc" {
		t.Fatalf("missing headers: %v", cfg.Headers)
	}
	// 模板字符串在源端求值后 ${num} 已是字面 6
	if !strings.Contains(cfg.ModifyImageJS, "const num = 6") {
		t.Fatalf("template not evaluated: %q", cfg.ModifyImageJS)
	}
}

var _ = assets.InitJS
var _ = time.Second
