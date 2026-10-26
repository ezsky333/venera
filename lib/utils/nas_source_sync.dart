import 'dart:io';

import 'package:venera/foundation/app.dart';
import 'package:venera/foundation/comic_source/comic_source.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';

/// 把 NAS 上已有的漫画源同步到本机。
///
/// 为什么需要这一步：
/// 新设备（平板/第二台手机/模拟器）刚装好 App 时**一个源都没有**。
/// 没有源，收藏列表渲染不出内容、阅读也打不开——用户看到的就是
/// "同步过来了但全是空的"。让用户手动导入 30 多个 JS 文件不现实。
///
/// 服务端为了跑下载本来就持有一份源脚本，直接下发即可：
///   - 两端跑同一版本，行为不会分叉；
///   - 客户端只需写文件 + reload，不需要理解源的内容。
///
/// 只补不覆盖：本机已有同名源（可能是用户自己更新的版本）时跳过，
/// 避免把用户手里的新版降级成服务端的旧版。
class NasSourceSync {
  /// 从 NAS 拉取源并补齐本地缺失的部分。
  ///
  /// 返回实际新增的源数量。任何失败都只记录日志、不影响主流程。
  static Future<int> syncFromNas({bool force = false}) async {
    final client = NasClient.instance;
    if (!client.isEnabled) return 0;
    if (!await client.probe()) return 0;

    final bundles = await client.sourceBundles();
    if (bundles.isEmpty) return 0;

    final dir = Directory("${App.dataPath}/comic_source");
    if (!await dir.exists()) {
      await dir.create(recursive: true);
    }

    // 本机已有哪些 key（用文件名判断，不必解析 JS）
    final existing = <String>{};
    await for (final e in dir.list()) {
      if (e is File && e.path.toLowerCase().endsWith('.js')) {
        final name = e.uri.pathSegments.last;
        existing.add(name.substring(0, name.length - 3));
      }
    }

    var added = 0;
    for (final b in bundles) {
      final key = b['key']?.toString() ?? '';
      final script = b['script']?.toString() ?? '';
      if (key.isEmpty || script.isEmpty) continue;

      // 已有且非强制更新 → 跳过（尊重用户本机的版本）
      if (existing.contains(key) && !force) continue;

      // 文件名必须安全：key 里出现路径分隔符会写到目录外
      final safe = key.replaceAll(RegExp(r'[\\/:*?"<>|]'), '_');
      final file = File("${dir.path}/$safe.js");
      try {
        await file.writeAsString(script);
        existing.add(key);
        added++;
      } catch (e) {
        Log.error('NAS Source', 'write $key failed: $e');
      }
    }

    if (added > 0) {
      // reload 会重新扫描目录并实例化所有源
      await ComicSourceManager().reload();
      Log.info('NAS Source', 'installed $added sources from NAS');
    }
    return added;
  }

  /// 启动时若本机一个源都没有，自动从 NAS 补齐。
  ///
  /// 只在"完全没有源"时自动执行：这是明确的异常状态（重装/新设备），
  /// 补全是用户期望的行为；而部分缺失交给设置页手动触发，
  /// 避免每次启动都做文件 IO。
  static Future<void> ensureSourcesOnStartup() async {
    if (!NasClient.instance.isEnabled) return;
    if (!ComicSourceManager().isEmpty) return;
    final n = await syncFromNas();
    if (n > 0) {
      Log.info('NAS Source', 'startup bootstrap: $n sources');
    }
  }
}

/// 供 UI 展示的源同步结果。
class NasSourceSyncResult {
  final int added;
  final String? error;
  const NasSourceSyncResult({this.added = 0, this.error});
}
