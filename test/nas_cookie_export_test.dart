import 'package:flutter_test/flutter_test.dart';
import 'package:venera/utils/nas_cookie_export.dart';

/// Cookie 域名匹配的回归测试。
///
/// 这里锁住两个真实事故：
///  1. 声明 `cdnutc.cc` 却匹配不到 `www.cdnutc.cc` —— jm / wnacg 的
///     登录态全部挂在 www 前缀下，结果一条都同步不出去；
///  2. 父域共享 Cookie（`.e-hentai.org`）必须能被 `e-hentai.org` 取到。
void main() {
  group('acceptedDomains', () {
    test('裸域会展开出 www 变体（jm 事故）', () {
      final acc = NasCookieExport.acceptedDomains(['cdnutc.cc']);
      expect(acc, contains('cdnutc.cc'));
      expect(acc, contains('www.cdnutc.cc'));
      expect(acc, contains('.cdnutc.cc'));
    });

    test('www 开头的声明也会展开出裸域', () {
      final acc = NasCookieExport.acceptedDomains(['www.wn05.cfd']);
      expect(acc, contains('www.wn05.cfd'));
      expect(acc, contains('wn05.cfd'));
    });

    test('多级子域会逐层展开父域', () {
      final acc = NasCookieExport.acceptedDomains(['forums.e-hentai.org']);
      expect(acc, contains('forums.e-hentai.org'));
      expect(acc, contains('.e-hentai.org'));
      expect(acc, contains('e-hentai.org'));
    });

    test('空串被忽略', () {
      final acc = NasCookieExport.acceptedDomains(['', '  ', 'a.com']);
      expect(acc, contains('a.com'));
      // 空串不应该制造出 '.' / 'www.' 这类垃圾条目
      expect(acc.where((e) => e.trim().isEmpty || e == '.' || e == 'www.'),
          isEmpty);
    });

    test('大小写不敏感', () {
      final acc = NasCookieExport.acceptedDomains(['NHentai.NET']);
      expect(acc, contains('nhentai.net'));
      expect(NasCookieExport.matches(acc, 'nhentai.net'), isTrue);
    });
  });

  group('matches', () {
    test('jm 的 www 前缀域名能命中（核心回归）', () {
      final acc = NasCookieExport.acceptedDomains(['cdnutc.cc']);
      expect(NasCookieExport.matches(acc, 'www.cdnutc.cc'), isTrue);
    });

    test('wnacg 的 CDN 分片域名能命中', () {
      final acc = NasCookieExport.acceptedDomains(['wnimg2.cfd', 'qy0.ru']);
      expect(NasCookieExport.matches(acc, 't1.wnimg2.cfd'), isTrue);
      expect(NasCookieExport.matches(acc, 'img4.wnimg2.cfd'), isTrue);
      expect(NasCookieExport.matches(acc, 't3.qy0.ru'), isTrue);
    });

    test('父域 Cookie（带前导点）能命中', () {
      final acc = NasCookieExport.acceptedDomains(['e-hentai.org']);
      expect(NasCookieExport.matches(acc, '.e-hentai.org'), isTrue);
      expect(NasCookieExport.matches(acc, 'e-hentai.org'), isTrue);
    });

    test('无关域名不会被误命中', () {
      final acc = NasCookieExport.acceptedDomains(['cdnutc.cc']);
      expect(NasCookieExport.matches(acc, 'evil-cdnutc.cc.example.com'),
          isFalse);
      expect(NasCookieExport.matches(acc, 'nhentai.net'), isFalse);
    });
  });

  group('ComicSourceDomains.of', () {
    test('大小写不一致的源 key 也能查到域名（komiic 事故）', () {
      final lower = ComicSourceDomains.of('komiic');
      final upper = ComicSourceDomains.of('Komiic');
      expect(lower, isNotEmpty);
      expect(upper, equals(lower));
    });

    test('jm 的域名表覆盖当前实际使用的 CDN', () {
      final jm = ComicSourceDomains.of('jm');
      expect(jm, contains('cdnutc.cc'));
      expect(jm, contains('cdnhth.club'));
      expect(jm, contains('cdngwc.cc'));
    });

    test('wnacg 的域名表覆盖 wnimg2.cfd', () {
      final w = ComicSourceDomains.of('wnacg');
      expect(w, contains('wnimg2.cfd'));
      expect(w, contains('qy0.ru'));
    });

    test('未知的源返回空列表而不是抛异常', () {
      expect(ComicSourceDomains.of('__no_such_source__'), isEmpty);
    });
  });
}
