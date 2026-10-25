import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/nas_client.dart';

void main() {
  group('NasConfig', () {
    test('normalize adds scheme and strips trailing slash', () {
      expect(NasConfig.normalize('192.168.1.10:8080'), 'http://192.168.1.10:8080');
      expect(NasConfig.normalize('http://host:8080/'), 'http://host:8080');
      expect(NasConfig.normalize('  https://nas.local  '), 'https://nas.local');
      expect(NasConfig.normalize(''), '');
    });

    test('normalize keeps existing scheme', () {
      expect(NasConfig.normalize('https://x.io/a/'), 'https://x.io/a');
    });

    test('round-trips through json', () {
      const cfg = NasConfig(
        baseUrl: 'http://nas:8080',
        token: 'abc',
        clientId: 'dev-1',
        cursor: 42,
      );
      final back = NasConfig.fromJson(cfg.toJson());
      expect(back.baseUrl, cfg.baseUrl);
      expect(back.token, cfg.token);
      expect(back.clientId, cfg.clientId);
      expect(back.cursor, cfg.cursor);
    });

    test('isValid reflects baseUrl presence', () {
      expect(const NasConfig(baseUrl: '', token: '', clientId: '').isValid, isFalse);
      expect(
        const NasConfig(baseUrl: 'http://x', token: '', clientId: '').isValid,
        isTrue,
      );
    });

    test('copyWith preserves unspecified fields', () {
      const cfg = NasConfig(
        baseUrl: 'http://a',
        token: 't',
        clientId: 'c',
        cursor: 1,
      );
      final updated = cfg.copyWith(cursor: 9);
      expect(updated.baseUrl, 'http://a');
      expect(updated.token, 't');
      expect(updated.cursor, 9);
    });
  });

  group('NasChapter', () {
    test('isAvailable requires non-empty pages', () {
      expect(const NasChapter(pages: []).isAvailable, isFalse);
      expect(const NasChapter(pages: ['u']).isAvailable, isTrue);
    });
  });

  group('NasClient 基础状态', () {
    test('starts disabled without config', () {
      // 未调用 load() 时是默认状态；不应抛异常
      expect(NasClient.instance.status, NasStatus.disabled);
      expect(NasClient.instance.isEnabled, isFalse);
    });

    test('queries degrade gracefully when disabled', () async {
      // 未配置时所有查询应安全返回空值，绝不抛异常
      expect(
        await NasClient.instance.findChapter(
          sourceKey: 'jm',
          comicId: '1',
          chapterId: '1',
        ),
        isNull,
      );
      expect(await NasClient.instance.cachedChapters('jm', '1'), isEmpty);
      expect(await NasClient.instance.library(), isEmpty);
      expect(await NasClient.instance.info(), isNull);
      expect(
        await NasClient.instance.requestDownload(sourceKey: 'jm', comicId: '1'),
        isNull,
      );
      expect(await NasClient.instance.probe(), isFalse);
    });
  });
}
