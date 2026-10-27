import 'package:flutter_test/flutter_test.dart';
import 'package:venera/utils/nas_sync.dart';

// 墓碑协议在客户端侧的解析验证。
//
// 背景：服务端拒绝一条"迟到的 fav.add"（该收藏已被别的设备删除）时，
// 会返回 warning 让推送方把本地那条删掉。这里验证解析逻辑——它决定
// 客户端到底删哪一条，解析错了会删错漫画或删不掉。
void main() {
  group('parseResurrectWarnings', () {
    test('普通 comicId', () {
      final r = parseResurrectWarnings([
        'favorite resurrect blocked: 禁漫/jm/1207843',
      ]);
      expect(r, hasLength(1));
      expect(r.first.folder, '禁漫');
      expect(r.first.source, 'jm');
      expect(r.first.comicId, '1207843');
    });

    test('comicId 是 URL（ehentai）时必须完整还原', () {
      // ehentai 的 id 就是 URL，内部含斜杠。简单 split 取第三段会得到
      // "https:"，导致删错/删不掉。
      final r = parseResurrectWarnings([
        'favorite resurrect blocked: '
            '默认/ehentai/https://e-hentai.org/g/123/abcdef/',
      ]);
      expect(r, hasLength(1));
      expect(r.first.comicId, 'https://e-hentai.org/g/123/abcdef/');
    });

    test('忽略无关警告', () {
      final r = parseResurrectWarnings([
        'queue download: already cached',
        'favorite add ignored: empty folder',
      ]);
      expect(r, isEmpty);
    });

    test('多条警告都要解析出来', () {
      final r = parseResurrectWarnings([
        'favorite resurrect blocked: A/jm/1',
        'other warning',
        'favorite resurrect blocked: B/picacg/2',
      ]);
      expect(r, hasLength(2));
      expect(r.map((e) => e.comicId), ['1', '2']);
    });

    test('格式不完整时跳过，不抛异常', () {
      expect(parseResurrectWarnings(['favorite resurrect blocked: 只有两段/jm']),
          isEmpty);
      expect(parseResurrectWarnings(['favorite resurrect blocked: ']), isEmpty);
    });

    test('非列表输入安全返回空', () {
      expect(parseResurrectWarnings(null), isEmpty);
      expect(parseResurrectWarnings('not a list'), isEmpty);
      expect(parseResurrectWarnings({'a': 1}), isEmpty);
    });

    test('文件夹名里带空格也要保留', () {
      final r = parseResurrectWarnings([
        'favorite resurrect blocked: 全彩 /jm/42',
      ]);
      expect(r.first.folder, '全彩 ');
      expect(r.first.comicId, '42');
    });
  });
}
