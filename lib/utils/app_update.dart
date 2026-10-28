import 'package:venera/foundation/log.dart';
import 'package:venera/network/app_dio.dart';

/// 更新检测指向的仓库。
///
/// 上游原版检查的是 `venera-app/venera`，两个地方不对：
///
///   1. 仓库错了——它比对上游的版本号，永远提示"有新版"，点了跳上游页面
///      下载到原版 App，NAS 那套改动全丢；
///   2. 渠道错了——它读 master 分支上的 pubspec.yaml，而本仓库是**打 tag
///      发布**（v1.6.6 之类）。分支上的版本号和 tag 不是一回事：
///      v1.6.6 那次发布，master 上的 pubspec 仍是 1.6.3。
///
/// 所以这里以**发布 tag** 为准，不看分支上的 pubspec。
/// 换仓库不用改代码：构建时 `--dart-define=UPDATE_REPO=owner/repo`。
const String updateRepo =
    String.fromEnvironment('UPDATE_REPO', defaultValue: 'ezsky333/venera');

/// 一次"查到的最新发布"。
class AppUpdateInfo {
  const AppUpdateInfo({required this.version, required this.url});

  /// 去掉 `v` 前缀的纯版本号，例如 `1.6.6`。
  final String version;

  /// 用户点"更新"时打开的地址。
  final String url;
}

/// 发布页地址（Release 列表，不绑定具体 tag）。
String get releasePageUrl => 'https://github.com/$updateRepo/releases';

/// 查最新发布。返回 null 表示查询失败（网络/限流/仓库不可达）。
///
/// 两个渠道都不抛异常：检查更新是后台动作，失败只该记日志，
/// 不能让设置页的按钮一直转圈或连弹错误。
Future<AppUpdateInfo?> fetchLatestRelease() async {
  final version = await _latestTagFromGithubApi() ?? await _latestTagFromJsDelivr();
  if (version == null) return null;
  return AppUpdateInfo(version: version, url: releasePageUrl);
}

/// 渠道一：GitHub Releases API。
///
/// 这是权威来源——它就是用户真正下载的那个页面，tag 与附件一一对应。
/// 缺点：匿名调用每小时 60 次配额，部分网络下不通，所以失败要能安静降级。
Future<String?> _latestTagFromGithubApi() async {
  try {
    final res = await AppDio().get(
      'https://api.github.com/repos/$updateRepo/releases/latest',
      options: Options(
        headers: {
          'Accept': 'application/vnd.github+json',
          // NetworkCacheManager 认这个头。不带的话同一次运行里第二次
          // 检查更新会吃到 5 秒内的内存缓存：用户刚装完新版再点
          // "检查更新"，仍然提示有新版。
          'cache-time': 'no',
        },
      ),
    );
    if (res.statusCode != 200) {
      Log.info('Check Update', 'github api returned ${res.statusCode}');
      return null;
    }
    final data = res.data;
    if (data is! Map) return null;
    return _normalizeTag(data['tag_name']?.toString() ?? '');
  } catch (e) {
    Log.info('Check Update', 'github api failed: $e');
    return null;
  }
}

/// 渠道二：jsDelivr 的包版本列表。
///
/// jsDelivr 为 GitHub 仓库按 tag 建索引，`versions[].version` 就是去掉 `v`
/// 的 tag。没有 API 配额、有 CDN 缓存，国内也能通。
Future<String?> _latestTagFromJsDelivr() async {
  try {
    final res = await AppDio().get(
      'https://data.jsdelivr.com/v1/packages/gh/$updateRepo',
      options: Options(headers: {'cache-time': 'no'}),
    );
    if (res.statusCode != 200) {
      Log.info('Check Update', 'jsdelivr returned ${res.statusCode}');
      return null;
    }
    final data = res.data;
    if (data is! Map) return null;
    final versions = data['versions'];
    if (versions is! List) return null;
    String? best;
    for (final item in versions) {
      final raw = item is Map ? item['version']?.toString() : null;
      if (raw == null) continue;
      final v = _normalizeTag(raw);
      if (v == null) continue;
      if (best == null || isNewerVersion(v, best)) {
        best = v;
      }
    }
    return best;
  } catch (e) {
    Log.info('Check Update', 'jsdelivr failed: $e');
    return null;
  }
}

/// 把 tag（`v1.6.6` / `1.6.6`）规整成纯版本号；不像版本号就返回 null。
///
/// jsDelivr 索引里可能混进非版本 tag（`nightly`、`latest` 之类），
/// 直接拿去 `int.parse` 会炸。
String? _normalizeTag(String raw) {
  var s = raw.trim();
  if (s.startsWith('v') || s.startsWith('V')) {
    s = s.substring(1);
  }
  final match = RegExp(r'^(\d+\.\d+(?:\.\d+)?)').firstMatch(s);
  return match?.group(1);
}

/// [latest] 是否比 [current] 新。
///
/// 逐段按数字比，缺段按 0 处理（`1.7` == `1.7.0`）。不能按字符串比，
/// 否则 `1.10` 会被判成小于 `1.9`。
bool isNewerVersion(String latest, String current) {
  final a = _segments(latest);
  final b = _segments(current);
  final n = a.length > b.length ? a.length : b.length;
  for (var i = 0; i < n; i++) {
    final x = i < a.length ? a[i] : 0;
    final y = i < b.length ? b[i] : 0;
    if (x != y) return x > y;
  }
  return false;
}

List<int> _segments(String version) {
  return version
      .split('.')
      .map((e) => int.tryParse(e) ?? 0)
      .toList(growable: false);
}
