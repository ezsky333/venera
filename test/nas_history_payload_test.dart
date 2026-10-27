import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/comic_type.dart';
import 'package:venera/foundation/history.dart';
import 'package:venera/utils/nas_sync.dart';

/// 锁住「history.update payload 必须能被 jsonEncode」这条硬约束。
///
/// 这条约束不是形式主义：push 是把整批 op 一次性序列化后发送的，
/// 只要有一个字段不可编码（Set / 自定义对象 / NaN…），
/// `dio.post` 在编码阶段就会抛 TypeError，
/// 同批次的收藏、设置、源登录态**全部发不出去**。
///
/// 真实事故：`readEpisode` 是 `Set<String>`，被直接塞进 payload，
/// 于是"收藏同步了、阅读进度永远停在旧值"——服务端只剩
/// `_bootstrapPush` 那次批量上传留下的记录（那边写的是 toList()）。
History _makeHistory() {
  final h = History.fromMap({
    'type': ComicType.local.value,
    'id': 'comic-1',
    'title': 'Test Comic',
    'subtitle': 'sub',
    'cover': 'https://img/1.jpg',
    'ep': 3,
    'page': 17,
    'max_page': 42,
    'time': 1700000000000,
    'readEpisode': ['1', '2', '3'],
  });
  h.group = 2;
  return h;
}

void main() {
  group('history.update payload', () {
    test('readEpisode(Set) 被序列化成 List，jsonEncode 不抛异常', () {
      final h = _makeHistory();
      // 前置条件：readEpisode 确实是 Set（这是问题的来源）
      expect(h.readEpisode, isA<Set<String>>());

      final payload = buildHistoryOpPayload(h);

      // 关键断言 1：字段类型是 List，不是 Set
      expect(payload['readEpisodes'], isA<List<dynamic>>());
      expect(payload['readEpisodes'], isNot(isA<Set<dynamic>>()));

      // 关键断言 2：整个 payload 能被编码（事故就发生在这里）
      late String encoded;
      expect(() => encoded = jsonEncode(payload), returnsNormally);

      final decoded = jsonDecode(encoded) as Map<String, dynamic>;
      expect(decoded['readEpisodes'], unorderedEquals(['1', '2', '3']));
      expect(decoded['comicId'], 'comic-1');
      expect(decoded['ep'], 3);
      expect(decoded['page'], 17);
      expect(decoded['group'], 2);
      expect(decoded['maxPage'], 42);
      expect(decoded['at'], 1700000000000);
    });

    test('空 readEpisode 编码成空数组而不是 null', () {
      final h = History.fromMap({
        'type': ComicType.local.value,
        'id': 'comic-2',
        'title': 'X',
        'subtitle': '',
        'cover': '',
        'ep': 1,
        'page': 1,
        'max_page': 1,
        'time': 1700000000000,
        'readEpisode': <String>[],
      });

      final payload = buildHistoryOpPayload(h);
      final decoded = jsonDecode(jsonEncode(payload)) as Map<String, dynamic>;
      expect(decoded['readEpisodes'], isEmpty);
      expect(decoded['readEpisodes'], isA<List<dynamic>>());
    });

    test('整批 op（收藏 + 历史 + 设置）一起编码不失败', () {
      // 复现真实的 flush 批次结构：一个不可编码字段会让整批挂掉。
      final ops = <Map<String, dynamic>>[
        {
          'type': 'fav.add',
          'payload': {
            'folder': 'Favorites',
            'source': 'jm',
            'comicId': 'c1',
            'title': 'T',
            'tags': ['a', 'b'],
            'sourceType': 1,
          },
        },
        {'type': 'history.update', 'payload': buildHistoryOpPayload(_makeHistory())},
        {
          'type': 'setting.update',
          'payload': {'key': 'readerMode', 'value': 'page'},
        },
      ];
      expect(() => jsonEncode({'ops': ops}), returnsNormally);
    });
  });
}
