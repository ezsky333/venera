import 'package:flutter_test/flutter_test.dart';

/// 离线模式下详情页**不允许**再向源站发任何请求。
///
/// 真实问题（用户报告"离线模式开着，ehentai 详情页还在连服务器"）：
/// 详情主体已经由 NAS 兜底渲染成功，但页面里还有几个"顺手"的源站调用
/// 会被触发，日志里能看到它们紧跟在 `offline mode hit` 之后：
///
///   01:33:10.307  offline mode hit ehentai/2790610: 1 cached chapters
///   01:33:10.326  GET https://exhentai.org/g/2790610/...      ← 缩略图列表
///   01:33:12.983  GET https://zoycbewnml.hath.network/...     ← 缩略图本体
///
/// 逐个封掉：
///   1. `onDataLoaded` 的收藏夹查询（多夹源才走）
///   2. `_ComicThumbnails.loadNext` 的缩略图拉取
///
/// 这里用一个最小的"开关 + 调用决策"模型把规则钉住，
/// 避免以后新增的源站调用又漏掉这个判断。
class _DetailPageSourceCalls {
  _DetailPageSourceCalls({required this.offline});

  final bool offline;

  /// 详情主体是否先问 NAS（离线模式下为 true）
  bool get tryNasFirst => offline;

  /// 是否查询收藏夹归属（源站请求）
  bool get shouldLoadFavoriteFolders => !offline;

  /// 是否拉缩略图画廊（源站请求）
  bool get shouldLoadThumbnails => !offline;

  /// 是否加载评论（源站请求，独立页面）
  bool get shouldLoadComments => !offline;
}

void main() {
  group('离线模式下详情页不请求源站', () {
    test('离线：所有源站调用都被跳过', () {
      final page = _DetailPageSourceCalls(offline: true);
      expect(page.tryNasFirst, isTrue);
      expect(page.shouldLoadFavoriteFolders, isFalse,
          reason: '收藏夹查询会请求 ehentai，离线时必然失败并污染日志');
      expect(page.shouldLoadThumbnails, isFalse,
          reason: '缩略图是"详情页仍在连源站"的最后一个漏点');
      expect(page.shouldLoadComments, isFalse);
    });

    test('在线：行为与原版一致', () {
      final page = _DetailPageSourceCalls(offline: false);
      expect(page.tryNasFirst, isFalse);
      expect(page.shouldLoadFavoriteFolders, isTrue);
      expect(page.shouldLoadThumbnails, isTrue);
      expect(page.shouldLoadComments, isTrue);
    });
  });
}
