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
  /// 把源声明的域名展开成"可接受的 Cookie domain 集合"。
  ///
  /// 抽成独立静态方法是为了可测：这段逻辑出过一次真实事故——
  /// 只按精确域 + 父域展开，导致 `www.cdnutc.cc` 上的 Cookie 匹配不上
  /// 声明为 `cdnutc.cc` 的源，jm/wnacg 整站登录态一条都同步不出去。
  static Set<String> acceptedDomains(List<String> domains) {
    final accepted = <String>{};
    for (final d in domains) {
      final host = d.trim().toLowerCase();
      // 先 trim 再判空：只有空白字符的项（"  "）trim 后就是空串，
      // 放进去会生成 "." / "www." 这类无意义条目，白白扩大匹配面。
      if (host.isEmpty) continue;
      accepted.add(host);
      // 同时接受 www. 变体。
      //
      // Cookie 是按精确 domain 存的，站点常常只往 `www.cdnutc.cc` 上写，
      // 而源脚本声明的是 `cdnutc.cc`。少了这一步，jm/wnacg 这类
      // 全部 cookie 都挂在 www 前缀域名下的站点会一条都同步不出去。
      if (!host.startsWith('www.')) {
        accepted.add('www.$host');
      } else {
        accepted.add(host.substring(4));
      }
      // 父域展开：`a.b.com` 也要能拿到 `.b.com` 的共享 Cookie
      final parts = host.split('.');
      for (var i = 0; i < parts.length - 1; i++) {
        final suffix = parts.sublist(i).join('.');
        accepted.add('.$suffix');
        accepted.add(suffix);
        accepted.add('www.$suffix');
      }
    }
    return accepted;
  }

  /// [domain] 是否属于 [accepted] 集合（忽略前导点方向差异）。
  ///
  /// 三个方向都要比，缺一个就会漏：
  ///   1. 原样命中：`e-hentai.org` vs `e-hentai.org`
  ///   2. 前导点差异：`.e-hentai.org` vs `e-hentai.org`
  ///   3. **子域命中**：`t1.wnimg2.cfd` 应当命中声明域 `wnimg2.cfd`
  ///
  /// 第 3 条是 jm / wnacg 事故的直接原因：它们的图片和接口分散在
  /// `t1.wnimg2.cfd`、`img4.wnimg2.cfd` 这类分片域名上，只比对父域方向
  /// （accepted 里有 `wnimg2.cfd`，但 cookie 是 `t1.wnimg2.cfd`）
  /// 会判定为不匹配，结果整站登录态一条都同步不出去。
  static bool matches(Set<String> accepted, String domain) {
    final d = domain.toLowerCase();
    final bare = d.startsWith('.') ? d.substring(1) : d;

    // 方向 1/2：原样与前导点差异
    if (accepted.contains(d) ||
        accepted.contains('.$d') ||
        accepted.contains(bare) ||
        accepted.contains('.$bare')) {
      return true;
    }

    // 方向 3：cookie 域名是某个声明域的子域
    final parts = bare.split('.');
    for (var i = 1; i < parts.length - 1; i++) {
      final suffix = parts.sublist(i).join('.');
      if (accepted.contains(suffix) || accepted.contains('.$suffix')) {
        return true;
      }
    }
    return false;
  }

  /// 导出属于 [domains] 之一的 Cookie。
  ///
  /// 匹配规则与 [CookieJarSql.loadForRequest] 一致（含父域），
  /// 这样 `exhentai.org` 拿得到 `.e-hentai.org` 的共享 Cookie。
  static List<Map<String, dynamic>> exportFor(List<String> domains) {
    final jar = SingleInstanceCookieJar.instance;
    if (jar == null || domains.isEmpty) return const [];

    final accepted = acceptedDomains(domains);

    try {
      final rows = jar.rawDb.select('''
        SELECT name, value, domain, path, expires, secure, httpOnly
        FROM cookies;
      ''');
      final out = <Map<String, dynamic>>[];
      for (final r in rows) {
        final domain = (r['domain'] as String?)?.toLowerCase() ?? '';
        if (!matches(accepted, domain)) continue;
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
    // jm 的图片和 API 走一堆 CDN 分片域名，且**换得很勤**：
    // 18comic.vip / jmcomic.me 这些老域名现在基本不用了，
    // 实际生效的是 cdnutc.cc / cdnhth.club / cdngwc.cc / cdntwice 等。
    // 这里保留老域名（老用户可能还在用）并补上新的，配合下方"同根域
    // 自动放宽"规则，``*.cdnutc.cc`` 这类分片也能覆盖到。
    'jm': [
      '18comic.vip',
      '18comic.org',
      'jmcomic.me',
      'jmcomic1.me',
      'cdntwice.org',
      'cdnutc.cc',
      'cdnhth.club',
      'cdngwc.cc',
      'cdnsha.org',
      'jmapiproxyxxx.vip',
    ],
    'picacg': [
      'picacomic.com',
      'storage1.picacomic.com',
      'storage-b.picacomic.com',
      'picacomicapis.com',
    ],
    // wnacg 同样是 CDN 分片全家桶（wnimg2.cfd / qy0.ru / wn05.cfd）
    'wnacg': [
      'wnacg.com',
      'wnacg01.link',
      'wnacg02.link',
      'wn05.cfd',
      'wnimg2.cfd',
      'qy0.ru',
    ],
    'copy_manga': ['copymanga.tv', 'copymanga.site', 'copymanga.info'],
    'komiic': ['komiic.com'],
    'hitomi': ['hitomi.la'],
    'manga_dex': ['mangadex.org', 'api.mangadex.org'],
    'comick': ['comick.io', 'comick.pictures'],
    'manhuagui': ['manhuagui.com', 'www.manhuagui.com'],
    'manhuaren': ['manhuaren.com', 'www.manhuaren.com'],
    'copy_manga_multi_accounts': [
      'copymanga.tv',
      'copymanga.site',
      'copymanga.info',
    ],
    'baozimh': ['baozimh.com', 'baozimh.org'],
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

  /// 源 key 到域名表**不区分大小写**。
  ///
  /// 真实存在这样的源：脚本里 `key` 声明成 `Komiic`（大写 K），
  /// 而安装时服务端按小写 `komiic` 落库，两边对不上就会被判成
  /// "source key mismatch" 直接装不上。域名查表同理，不能要求
  /// 大小写完全一致。
  static List<String> of(String sourceKey) {
    final exact = _map[sourceKey];
    if (exact != null) return exact;
    final lower = sourceKey.toLowerCase();
    for (final entry in _map.entries) {
      if (entry.key.toLowerCase() == lower) return entry.value;
    }
    return const <String>[];
  }
}
