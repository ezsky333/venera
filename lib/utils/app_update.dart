import 'package:yaml/yaml.dart';

import 'package:venera/foundation/log.dart';
import 'package:venera/network/app_dio.dart';

/// 更新检测指向的仓库。
///
/// 上游原版检查的是 `venera-app/venera`：读它 master 上的 pubspec.yaml，
/// 再把人送去 `venera-app/venera/releases`。放在本 fork 上这两处都得换，
/// 否则永远提示"有新版"，点进去下载回来的是原版 App，NAS 那套改动全丢。
///
/// 检测方式保持与上游一致——**读分支上 pubspec.yaml 里的 version**，
/// 不引入 tag 推导：测试阶段版本号本来就是随手改的，只要能判断
/// "远端比本地新"就够了，多一层 tag→版本的映射只会多一个出错的地方。
///
/// 换仓库不用改代码：构建时 `--dart-define=UPDATE_REPO=owner/repo`。
const String updateRepo =
    String.fromEnvironment('UPDATE_REPO', defaultValue: 'ezsky333/venera');

/// 读哪个分支上的 pubspec.yaml。
const String updateBranch =
    String.fromEnvironment('UPDATE_BRANCH', defaultValue: 'master');

/// 一次"查到的远端版本"。
class AppUpdateInfo {
  const AppUpdateInfo({required this.version, required this.url});

  /// 纯版本号，不含 build 号，例如 `1.6.5`。
  final String version;

  /// 用户点"更新"时打开的地址。
  final String url;
}

/// 发布页地址（Release 列表，不绑定具体 tag）。
String get releasePageUrl => 'https://github.com/$updateRepo/releases';

/// 查远端最新版本。返回 null 表示查询失败（网络/仓库不可达/内容不是 yaml）。
///
/// 不抛异常：检查更新是后台动作，失败只该记日志，不能让设置页的按钮
/// 一直转圈，也不该弹一串错误。
Future<AppUpdateInfo?> fetchLatestVersion() async {
  final version = await _versionFromJsDelivr() ?? await _versionFromRaw();
  if (version == null) return null;
  return AppUpdateInfo(version: version, url: releasePageUrl);
}

/// 主渠道：jsDelivr 的 GitHub 回源。
///
/// 有 CDN 缓存、无 API 配额，国内直连也能通；比 raw.githubusercontent
/// 稳得多（后者时不时整段超时）。
Future<String?> _versionFromJsDelivr() async {
  return _readPubspecVersion(
    'https://cdn.jsdelivr.net/gh/$updateRepo@$updateBranch/pubspec.yaml',
    'jsdelivr',
  );
}

/// 备用渠道：GitHub raw。
Future<String?> _versionFromRaw() async {
  return _readPubspecVersion(
    'https://raw.githubusercontent.com/$updateRepo/$updateBranch/pubspec.yaml',
    'raw',
  );
}

Future<String?> _readPubspecVersion(String url, String label) async {
  try {
    final res = await AppDio().get(
      url,
      options: Options(headers: {
        // NetworkCacheManager 认这个头。不带的话同一次运行里第二次检查
        // 更新会吃到 5 秒内的内存缓存：用户刚换到新版再点"检查更新"，
        // 仍然提示有新版本。
        'cache-time': 'no',
      }),
    );
    if (res.statusCode != 200) {
      Log.info('Check Update', '$label returned ${res.statusCode}');
      return null;
    }
    final text = res.data?.toString();
    if (text == null || text.isEmpty) return null;
    final data = loadYaml(text);
    final raw = data is YamlMap ? data['version']?.toString() : null;
    if (raw == null || raw.isEmpty) return null;
    // pubspec 的写法是 1.6.5+165，只取前面那段语义化版本
    return raw.split('+').first.trim();
  } catch (e) {
    Log.info('Check Update', '$label failed: $e');
    return null;
  }
}

/// [latest] 是否比 [current] 新。
///
/// 逐段按数字比，缺段按 0 处理（`1.7` == `1.7.0`）。不能按字符串比，
/// 否则 `1.10` 会被判成小于 `1.9`——上游原来的实现就是按字符串比的。
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
