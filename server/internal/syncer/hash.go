package syncer

import (
	darthash "github.com/venera-app/venera-nas/internal/hash"
)

// hashOf 返回与 App 兼容的 sourceKey.hashCode。
//
// 注意：这里只作为"客户端没上报时的兜底"。正确顺序是：
//  1. 查 store 的 source_type_map（由客户端首次上报，长期权威）
//  2. 查不到才现场计算
//
// 之所以要映射表，是因为 Dart 的 String.hashCode 属于实现细节，
// 未来可能变化；由客户端上报可保证永远匹配。
func hashOf(sourceKey string) int32 {
	return darthash.String(sourceKey)
}
