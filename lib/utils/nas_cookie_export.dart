import 'package:venera/foundation/log.dart';
import 'package:venera/network/cookie_jar.dart';

/// 把 App 的 Cookie 库导出成可同步到 NAS 的结构。
///
/// 为什么必须同步 Cookie：
/// 很多源的登录态**不止一个 token 字段**。ehentai 就是典型——
/// 它靠 `ipb_member_id` / `ipb_pass_hash` / `igneous` 三个 Cookie 认证，
/// 账号密码本身不参与后续请求。只同步 `<key>.data` 里的 account 字符串，
/// 服务端拿到的是一具空壳：请求会被重定向到登录页，表现为 302/404。
///
/// 这里按源声明的域名过滤，避免把 A 站的 Cookie 灌给 B 站。
class NasCookieExport {
  /// 导出属于 [domains] 之一的 Cookie。
  ///
  /// 匹配规则与 [CookieJarSql.loadForRequest] 一致（含父域），
  /// 这样 `exhentai.org` 拿得到 `.e-hentai.org` 的共享 Cookie。
  static List<Map<String, dynamic>> exportFor(List<String> domains) {
    final jar = SingleInstanceCookieJar.instance;
    if (jar == null || domains.isEmpty) return const [];

    final accepted = <String>{};
    for (final d in domains) {
      if (d.isEmpty) continue;
      final host = d.trim().toLowerCase();
      accepted.add(host);
      final parts = host.split('.');
      for (var i = 0; i < parts.length - 1; i++) {
        accepted.add('.${parts.sublist(i).join('.')}');
      }
    }

    try {
      final rows = jar.rawDb.select('''
        SELECT name, value, domain, path, expires, secure, httpOnly
        FROM cookies;
      ''');
      final out = <Map<String, dynamic>>[];
      for (final r in rows) {
        final domain = (r['domain'] as String?)?.toLowerCase() ?? '';
        if (!accepted.contains(domain) && !accepted.contains('.$domain')) {
          continue;
        }
        out.add({
          'name': r['name'],
          'value': r['value'],
          'domain': r['domain'],
          'path': r['path'] ?? '/',
          'expires': r['expires'],
          'secure': r['secure'] == 1,
          'httpOnly': r['httpOnly'] == 1,
        });
      }
      return out;
    } catch (e) {
      Log.error('NAS Sync', 'export cookies failed: $e');
      return const [];
    }
  }

  /// 某个源应当被同步的 Cookie。
  ///
  /// 域名来源两处：
  ///  1. 源脚本声明过的认证域名（见 [ComicSourceDomains]）；
  ///  2. 源数据里出现的 URL（兜底自建站，如 komga/kavita 的 base_url）。
  static List<Map<String, dynamic>> exportForSource(
    String sourceKey,
    Map<String, dynamic> data,
  ) {
    final domains = <String>{};
    domains.addAll(ComicSourceDomains.of(sourceKey));
    domains.addAll(_hostsIn(data));
    return exportFor(domains.toList());
  }

  /// 从源数据里挖出所有 host（base_url 之类的自定义地址）。
  static Set<String> _hostsIn(Map<String, dynamic> data) {
    final out = <String>{};
    void walk(dynamic v) {
      if (v is String) {
        final m = RegExp(r'https?://([^/\s:]+)').firstMatch(v);
        if (m != null) out.add(m.group(1)!.toLowerCase());
      } else if (v is Map) {
        for (final e in v.values) {
          walk(e);
        }
      } else if (v is List) {
        for (final e in v) {
          walk(e);
        }
      }
    }

    walk(data);
    return out;
  }
}

/// 各源的认证域名。
///
/// 只列**必须**的（决定登录态的域名），不追求完整：
/// 多列一个域名只是多同步几条 Cookie，少列则会导致服务端登录态失效。
class ComicSourceDomains {
  static const Map<String, List<String>> _map = {
    'ehentai': [
      'e-hentai.org',
      'exhentai.org',
      'forums.e-hentai.org',
      'sadpanda.us',
    ],
    'nhentai': ['nhentai.net'],
    'jm': ['18comic.vip', 'jmcomic.me', 'cdntwice.org'],
    'picacg': ['picacomic.com', 'storage1.picacomic.com'],
    'wnacg': ['wnacg.com', 'wnacg01.link', 'wnacg02.link'],
    'hitomi': ['hitomi.la'],
    'manga_dex': ['mangadex.org', 'api.mangadex.org'],
    'comick': ['comick.io', 'comick.pictures'],
    'manhuagui': ['manhuagui.com', 'www.manhuagui.com'],
    'manhuaren': ['manhuaren.com', 'www.manhuaren.com'],
    'copymanga': ['copymanga.tv', 'copymanga.site', 'copymanga.info'],
    'copy_manga': ['copymanga.tv', 'copymanga.site', 'copymanga.info'],
    'copy_manga_multi_accounts': [
      'copymanga.tv',
      'copymanga.site',
      'copymanga.info',
    ],
    'baozimh': ['baozimh.com', 'baozimh.org'],
    'komiic': ['komiic.com'],
    'mycomic': ['mycomic.com'],
    'godamh': ['godamh.com'],
    'ykmh': ['ykmh.com'],
    'hcomic': ['h-comic.com', 'hcomic.xyz'],
    'hot_manga': ['hotmanga.net'],
    'jcomic': ['jcomic.net'],
    'mh1234': ['mh1234.com'],
    'mh18': ['18mh.org'],
    'mxs': ['mxs.cc'],
    'zaimanhua': ['zaimanhua.com'],
    'baihehui': ['baihehui.com'],
    'ccc': ['ccc.ac'],
    'happy': ['happy-comic.com'],
    'manwaba': ['manwaba.com'],
    'shonen_jump_plus': ['shonenjumpplus.com'],
  };

  static List<String> of(String sourceKey) =>
      _map[sourceKey] ?? const <String>[];
}
