import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/nas_chapter_cache.dart';

// NasChapterCache 的缓存匹配逻辑验证。
//
// 背景：画廊型源（wnacg 等）没有章节列表，服务端下载时把整本存成
// chapter_id == comicId 的单条记录。客户端若只按真实章节 id 比对，
// 就永远匹配不上 —— 表现为"已缓存却显示未缓存、还能反复触发下载"
//（用户实测《契薬交唇》153 页 233MB 已缓存，详情页仍提示未缓存）。
void main() {
  group('NasChapterCache.isCached', () {
    test('真实章节 id 命中时返回 true', () {
      final c = NasChapterCache(sourceKey: 'wnacg', comicId: '341770');
      c.replaceCached({'abc123'});
      expect(c.isCached('abc123'), isTrue);
      expect(c.isCached('other'), isFalse);
    });

    test('服务端"整本"记录应被任意章节命中', () {
      final c = NasChapterCache(sourceKey: 'wnacg', comicId: '341770');
      c.replaceCached({'341770'});

      // 单章场景：UI 用 comicId 查
      expect(c.isCached('341770'), isTrue);
      // 多章场景：UI 用真实章节 id 查，也必须命中
      expect(c.isCached('some-real-chapter-id'), isTrue,
          reason: '整本已缓存时，任何章节都应视为已缓存');
      expect(c.hasWholeBookRecord, isTrue);
    });

    test('空集合时一律未缓存', () {
      final c = NasChapterCache(sourceKey: 'wnacg', comicId: '341770');
      c.replaceCached({});
      expect(c.isCached('341770'), isFalse);
      expect(c.isCached('anything'), isFalse);
      expect(c.hasWholeBookRecord, isFalse);
    });

    test('多章记录与整本记录共存', () {
      final c = NasChapterCache(sourceKey: 'jm', comicId: '594763');
      c.replaceCached({'594763', 'ch1', 'ch2'});
      expect(c.isCached('ch1'), isTrue);
      expect(c.isCached('ch2'), isTrue);
      expect(c.isCached('ch3'), isTrue, reason: '整本记录在，未单独缓存的章节也算');
    });
  });
}
