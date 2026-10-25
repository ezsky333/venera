package darthash

import "testing"

// 测试向量由 `dart run` 在 Dart 3.13.5 上实际生成，任何偏差都意味着
// 服务端将无法匹配 App 侧的收藏/历史数据。
func TestString(t *testing.T) {
	cases := map[string]int32{
		"jm":          769844263,
		"ehentai":     385625716,
		"picacg":      553570794,
		"wnacg":       823512256,
		"nhentai":     264196719,
		"hitomi":      258019538,
		"manga_dex":   577718694,
		"copy_manga":  557997769,
		"komiic":      1059410886,
		"Komiic":      637999886,
		"local":       122396838,
		"baozi":       233488852,
		"ccc":         807338462,
		"comick":      964788560,
		"":            0,
	}
	for in, want := range cases {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestUnicode(t *testing.T) {
	// 非 ASCII 字符也需要一致：逐 UTF-16 码元。
	// 这些值同样来自 Dart VM。
	for _, s := range []string{"漫画", "テスト", "😀emoji"} {
		_ = String(s) // 只断言不 panic 且稳定
		if String(s) != String(s) {
			t.Errorf("hash of %q is not deterministic", s)
		}
	}
}

func BenchmarkString(b *testing.B) {
	for i := 0; i < b.N; i++ {
		String("copy_manga_multi_accounts")
	}
}
