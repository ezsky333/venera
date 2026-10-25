// Package darthash 复现 Dart VM 的 String.hashCode 算法。
//
// Venera App 在收藏夹(local_favorite.db)与阅读历史(history.db)中，把漫画来源
// 存储为 `sourceKey.hashCode`（一个 30 位有符号整数），而不是字符串。
// 服务端若要匹配 App 侧的数据，必须逐位复现同一算法。
//
// 算法来自 Dart VM (runtime/vm/object.cc, String::Hash)：Jenkins 单向哈希，
// 逐 UTF-16 code unit 累加，最后取 30 位。
//
// 注意：这是对实现细节的依赖。若 Dart 未来更换算法，这里会静默失效。
// 因此上层（sync/resolver）还需维护一张持久化的 sourceKey -> int 映射表作为兜底。
package darthash

// String 返回与 Dart 中 `s.hashCode` 相同的值。
//
// Dart 的字符串是不可变 UTF-16 序列；Go 的 string 是 UTF-8 字节序列，
// 因此这里按 rune 迭代并展开为 UTF-16 码元（补充平面字符会产生两个码元）。
func String(s string) int32 {
	var h uint32
	for _, r := range s {
		if r <= 0xFFFF {
			h = addCodeUnit(h, uint32(r))
		} else {
			// 补充平面：转为代理对
			r -= 0x10000
			h = addCodeUnit(h, uint32(0xD800+(r>>10)))
			h = addCodeUnit(h, uint32(0xDC00+(r&0x3FF)))
		}
	}
	h += h << 3
	h ^= h >> 11
	h += h << 15
	return int32(h & 0x3FFFFFFF)
}

// addCodeUnit 是 String::Hash 的内层循环单步。
func addCodeUnit(h, c uint32) uint32 {
	h += c
	h += h << 10
	h ^= h >> 6
	return h
}

// StringSigned 返回带符号的 hashCode（Dart 的 int 在 Web 端为 32 位有符号，
// 在 VM 端为任意精度；历史库里存的是 30 位结果，故二者等价）。
func StringSigned(s string) int64 {
	return int64(String(s))
}
