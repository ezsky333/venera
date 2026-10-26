import 'dart:async';

import 'package:venera/foundation/appdata.dart';
import 'package:venera/foundation/log.dart';

/// 跨设备同步 UI 结构类设置。
///
/// 背景：收藏和历史同步过来后，pad 上「探索」页仍然显示"没有探索页面"、
/// 「分类」页显示"没有分类页面"。原因是这两页的内容**不由收藏决定**，
/// 而是由 `appdata.settings` 里的几个列表决定：
///
///   explore_pages   探索页有哪些 tab（源声明的 explorePages 标题）
///   categories      分类页有哪些 tab（源的 categoryData.key）
///   favorites       网络收藏有哪些 tab（源的 favoriteData.key）
///   searchSources   聚合搜索默认勾选哪些源
///
/// 这些是"我在手机上配好的使用习惯"，不是设备特有信息，
/// 应当跟随账号走。WebDAV 的整包备份本来就包含它们
/// （appdata.json 里除了 _disableSync 白名单外的所有键）。
///
/// 本类只处理上述四个列表 + 少量明确的设备无关偏好，
/// 不碰 proxy / deviceId / nas 这类设备级配置。
class NasSettingsSync {
  /// 需要同步的设置键（顺序无关）。
  ///
  /// 判断标准与用户确认过的一致：**只排除设备特有配置**，
  /// 其余尽可能同步。多设备是同一个人在用，
  /// "我在手机上开了某个探索页，平板上应该也能看到"才是预期。
  static const syncedKeys = <String>[
    // --- 界面结构（探索/分类/搜索/网络收藏的 tab 清单）---
    'explore_pages',
    'categories',
    'favorites',
    'searchSources',
    // --- 阅读器偏好（跟人走，不跟机器走）---
    'readerMode',
    'reverseChapterOrder',
    'enablePageAnimation',
    'enableTapToTurnPages',
    'reverseTapToTurnPages',
    'preloadImageCount',
    'showPageNumberInReader',
    'showSystemStatusBar',
    'limitImageWidth',
    'autoPageTurningInterval',
    'readerScrollSpeed',
    'readerScreenPicNumberForLandscape',
    'readerScreenPicNumberForPortrait',
    'enableLongPressToZoom',
    'longPressZoomPosition',
    'enableTurnPageByVolumeKey',
    'enableClockAndBatteryInfoInReader',
    'showSingleImageOnFirstPage',
    // --- 列表与收藏行为 ---
    'comicDisplayMode',
    'comicListDisplayMode',
    'comicTileScale',
    'newFavoriteAddTo',
    'moveFavoriteAfterRead',
    'localFavoritesFirst',
    'autoCloseFavoritePanel',
    'showFavoriteStatusOnTile',
    'showHistoryStatusOnTile',
    'followUpdatesFolder',
    'initialPage',
    'onClickFavorite',
    'quickFavorite',
    'quickCollectImage',
    'showChapterComments',
    'showChapterCommentsAtEnd',
    'comicSpecificSettings',
    // --- 内容过滤（跟人走）---
    'blockedWords',
    'blockedCommentWords',
    'autoAddLanguageFilter',
    // --- 主题与语言（用户要求尽量同步；本机可在设置页单独改回来）---
    'color',
    'theme_mode',
    'language',
  ];

  /// 明确**不同步**的键。列在这里是为了让"为什么不同步"可查，
  /// 而不是散落在各处的隐式约定。
  static const deviceOnlyKeys = <String>[
    'proxy', // 代理地址绑定到具体网络环境
    'authorizationRequired', // 本机生物识别
    'customImageProcessing', // 可能引用本机脚本路径
    'webdav', // WebDAV 账号，各设备可不同
    'disableSyncFields', // 同步排除清单本身
    'deviceId', // 设备标识
    'nas', // NAS 地址与 token，每台设备各自配置
    'nasClientId', // 设备级客户端 ID
    'cacheSize', // 本机缓存上限（存储容量相关）
    'downloadThreads', // 本机下载并发（性能相关）
    'enableDnsOverrides',
    'dnsOverrides', // DNS 覆盖通常针对本地网络
    'sni',
    'ignoreBadCertificate', // 安全策略，按设备风险承受度设
    'comicSourceListUrl', // 源列表地址，可由用户在单机决定
    'dataVersion', // 整包版本号，NAS 用 op-log 游标，无意义
    'checkUpdateOnStart',
    'lastCheckUpdate',
  ];

  /// 收集本机当前值得同步的设置项。
  ///
  /// **不推空值**：新装的 App 里 explore_pages/categories 都是空数组，
  /// 把"我从没配过"当成"我要清空"推上去，会把别的设备的配置抹掉。
  ///
  /// 至于"非空的出厂默认值"（如 readerMode=galleryLeftToRight），
  /// 由**先拉后推**的顺序解决：拉取阶段已经用服务端的值覆盖了本地，
  /// 所以随后的推送带上去的就是远端那套配置，不会互相打架。
  static Map<String, dynamic> collect() {
    final out = <String, dynamic>{};
    for (final k in syncedKeys.toSet()) {
      if (deviceOnlyKeys.contains(k)) continue;
      final v = appdata.settings[k];
      if (v == null) continue;
      if (_isEmpty(v)) continue;
      out[k] = v;
    }
    return out;
  }

  /// 只把结构性为空的东西算空（空数组/空串/空 map）。
  /// `false` 与 `0` 是有意义的用户选择，不算空。
  static bool _isEmpty(dynamic v) {
    if (v == null) return true;
    if (v is String) return v.isEmpty;
    if (v is List) return v.isEmpty;
    if (v is Map) return v.isEmpty;
    return false;
  }

  /// 应用来自远端的设置。
  ///
  /// 策略是**覆盖**而非合并：这些列表表达的是"最终状态"，
  /// 合并两个设备的 tab 清单会产生没人想要的并集。
  /// 逐键比较是为了避免无谓的 saveData 与后续上传循环。
  static Future<int> apply(Map<String, dynamic> remote) async {
    var changed = 0;
    for (final entry in remote.entries) {
      final k = entry.key;
      if (deviceOnlyKeys.contains(k)) continue;
      if (!syncedKeys.contains(k) && !k.startsWith('comicSpecific')) continue;

      final cur = appdata.settings[k];
      final next = entry.value;
      if (_deepEquals(cur, next)) continue;

      appdata.settings[k] = next;
      changed++;
    }
    if (changed > 0) {
      try {
        await appdata.saveData(false);
      } catch (e) {
        Log.error('NAS Settings', 'save failed: $e');
      }
    }
    return changed;
  }

  /// 把本机设置推成一批 setting.update 操作。
  static List<Map<String, dynamic>> buildOps() {
    final ops = <Map<String, dynamic>>[];
    for (final entry in collect().entries) {
      ops.add({
        'type': 'setting.update',
        'payload': {'key': entry.key, 'value': entry.value},
      });
    }
    return ops;
  }

  static bool _deepEquals(dynamic a, dynamic b) {
    if (identical(a, b)) return true;
    if (a is List && b is List) {
      if (a.length != b.length) return false;
      for (var i = 0; i < a.length; i++) {
        if (!_deepEquals(a[i], b[i])) return false;
      }
      return true;
    }
    if (a is Map && b is Map) {
      if (a.length != b.length) return false;
      for (final k in a.keys) {
        if (!b.containsKey(k)) return false;
        if (!_deepEquals(a[k], b[k])) return false;
      }
      return true;
    }
    return a == b;
  }
}
