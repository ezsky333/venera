import 'dart:io';

import 'package:venera/foundation/app.dart';
import 'package:venera/foundation/comic_source/comic_source.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/utils/nas_cookie_export.dart';

/// 漫画源的跨设备同步。
///
/// 语义是"**装了什么就同步什么**"：
///   - 手机上装了哪几个源，就把这几个源的脚本推给服务端；
///   - 服务端只持有这些源，下载队列也只会涉及它们；
///   - 新设备（pad）从服务端拉取同样的那几个源。
///
/// 这与"把 venera-configs 里几十个源全量下发"是两种不同的语义：
/// 后者会让每台设备都凭空多出用户根本没启用的源，
/// 探索页/搜索页也因此被塞满噪声。
///
/// 同步内容对应本地 `comic_source/` 目录：
///   - `<key>.js` 源脚本本身
///   - 源的登录态（走 `source.data` + `source.cookie` 两种 op，
///     与既有的登录态同步链路共用，避免两套机制打架）
class NasSourceSync {
  /// 把**本机已安装的源脚本**推送到服务端。
  ///
  /// 返回推送成功的源数量。单个源失败不影响其余。
  static Future<int> pushToNas() async {
    final client = NasClient.instance;
    if (!client.isEnabled) return 0;
    if (!await client.probe()) return 0;

    final dir = Directory("${App.dataPath}/comic_source");
    if (!await dir.exists()) return 0;

    // 服务端已有的源版本，用于跳过未变更的（避免每次启动都传几百 KB）
    final remote = await client.sourceManifest();

    final ops = <Map<String, dynamic>>[];
    await for (final e in dir.list()) {
      if (e is! File) continue;
      final name = e.uri.pathSegments.last;
      if (!name.toLowerCase().endsWith('.js')) continue;

      final key = name.substring(0, name.length - 3);
      // 下划线开头的是模板/宿主脚本（_template_.js / _venera_.js），不是源
      if (key.startsWith('_')) continue;

      String script;
      try {
        script = await e.readAsString();
      } catch (err) {
        Log.error('NAS Source', 'read $name failed: $err');
        continue;
      }

      // 版本相同就不重复上传（服务端已经有一模一样的脚本）
      final localVersion = _metaOf(script, 'version');
      final remoteVersion = remote[key];
      if (remoteVersion != null && remoteVersion == localVersion && localVersion.isNotEmpty) {
        continue;
      }

      ops.add({
        'type': 'source.script',
        'payload': {
          'source': key,
          'name': _metaOf(script, 'name'),
          'version': localVersion,
          'script': script,
        },
      });
    }

    if (ops.isEmpty) return 0;

    // 分批推：单个 op 里塞几十个脚本会让请求体过大，
    // 也不利于失败时定位是哪一批出的问题。
    const batchSize = 4;
    var sent = 0;
    for (var i = 0; i < ops.length; i += batchSize) {
      final batch = ops.sublist(i, (i + batchSize).clamp(0, ops.length));
      final ok = await client.pushOps(batch);
      if (!ok) {
        Log.error('NAS Source', 'push batch failed at offset $i');
        break;
      }
      sent += batch.length;
    }
    if (sent > 0) {
      Log.info('NAS Source', 'pushed $sent source scripts to NAS');
    }
    return sent;
  }

  /// 从 NAS 补齐本机缺失的源脚本。
  ///
  /// [force] 为 true 时连本机已有的同名源也覆盖（用户手动点「同步源」的场景）。
  static Future<int> pullFromNas({bool force = false}) async {
    final client = NasClient.instance;
    if (!client.isEnabled) return 0;
    if (!await client.probe()) return 0;

    final bundles = await client.sourceBundles();
    if (bundles.isEmpty) return 0;

    final dir = Directory("${App.dataPath}/comic_source");
    if (!await dir.exists()) {
      await dir.create(recursive: true);
    }

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
      if (existing.contains(key) && !force) continue;

      final safe = _safeFileName(key);
      try {
        await File("${dir.path}/$safe.js").writeAsString(script);
        existing.add(key);
        added++;
      } catch (e) {
        Log.error('NAS Source', 'write $key failed: $e');
      }
    }

    if (added > 0) {
      await ComicSourceManager().reload();
      Log.info('NAS Source', 'installed $added sources from NAS');
    }
    return added;
  }

  /// 把服务端同步过来的源私有数据合并进本地源。
  ///
  /// 服务端存的是**合并后**的结果（多台设备的设置叠加）。
  /// 直接覆盖本地会丢掉本机独有的字段，因此按 key 合并且**本地优先**：
  ///   - 本地没有该键 → 采用远端值（这才让新设备"登录上了"）
  ///   - 本地已有该键 → 保留本地（登录态以本机为准，避免被别的设备顶掉）
  ///
  /// 只读的 UI 偏好类设置不在此处处理 —— 那些走 setting.update 的常规链路。
  static Future<int> applySourceData(
    Map<String, Map<String, dynamic>> remote,
  ) async {
    var applied = 0;
    for (final entry in remote.entries) {
      final source = ComicSource.find(entry.key);
      if (source == null) continue; // 源还没装，等下一轮再合并

      final local = source.data;
      var changed = false;
      for (final kv in entry.value.entries) {
        if (local[kv.key] == null) {
          local[kv.key] = kv.value;
          changed = true;
        }
      }
      if (!changed) continue;

      try {
        await source.saveData();
        applied++;
      } catch (e) {
        Log.error('NAS Source', 'save data for ${entry.key} failed: $e');
      }
    }
    return applied;
  }

  /// 启动时若本机一个源都没有，自动从 NAS 补齐。
  ///
  /// 只在"完全没有源"时自动执行：这是重装/新设备的明确特征。
  /// 部分缺失交给设置页手动触发，避免每次启动都做文件 IO。
  static Future<void> ensureSourcesOnStartup() async {
    if (!NasClient.instance.isEnabled) return;
    if (!ComicSourceManager().isEmpty) return;
    final n = await pullFromNas();
    if (n > 0) {
      Log.info('NAS Source', 'startup bootstrap: $n sources');
    }
  }

  /// 从脚本源码里读某个顶层字段的字符串值。
  ///
  /// 不用正则一次性抓全部字段，是因为要处理引号和转义；
  /// 这里只需要 key/name/version，逐个精确匹配即可。
  static String _metaOf(String js, String field) {
    final re = RegExp(
      '^\\s*$field\\s*[=:]\\s*(["' "'" r'])(.*?)\1',
      multiLine: true,
    );
    return re.firstMatch(js)?.group(2) ?? '';
  }

  /// 把 key 转成安全文件名（key 来自另一端设备，属不可信输入）。
  static String _safeFileName(String key) {
    final s = key.replaceAll(RegExp(r'[\\/:*?"<>|\x00-\x1f]'), '_').trim();
    if (s.isEmpty) return 'source';
    return s.length > 100 ? s.substring(0, 100) : s;
  }

  /// 供设置页使用：一次性完成"上传本地源 → 下载缺失源"的双向对齐。
  static Future<String> fullSync() async {
    final pushed = await pushToNas();
    final pulled = await pullFromNas();
    return 'up=$pushed down=$pulled';
  }
}

/// 让外部（设置页）能拿到 Cookie 导出的能力，用于展示/诊断。
List<Map<String, dynamic>> exportCookiesFor(
  String sourceKey,
  Map<String, dynamic> data,
) {
  return NasCookieExport.exportForSource(sourceKey, data);
}
