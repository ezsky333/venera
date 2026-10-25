import 'dart:async';
import 'dart:io' show Platform;

import 'package:flutter/foundation.dart';

import 'package:venera/foundation/comic_source/comic_source.dart';
import 'package:venera/foundation/comic_type.dart';
import 'package:venera/foundation/favorites.dart';
import 'package:venera/foundation/history.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/network/app_dio.dart';

/// W2 同步引擎（App 侧）。
///
/// 与原有 `DataSync`（WebDAV 整包覆盖）的区别：
///   - **操作日志**：只上传"我做了什么"（新增/删除收藏、进度更新），不传整包；
///   - **游标增量**：记录服务端 op id，重连只拉之后的操作；
///   - **语义合并**：收藏是集合、历史是 LWW，天然支持多设备；
///   - **双向**：服务端也会把其他设备的改动推给本机。
///
/// 设计上刻意做成"旁路"：原 DataSync 完全不动，用户可任选其一或都不用。
class NasSync with ChangeNotifier {
  static NasSync? _instance;

  factory NasSync() => _instance ?? NasSync._create();

  NasSync._create();

  bool _isSyncing = false;

  bool get isSyncing => _isSyncing;

  String? _lastError;

  String? get lastError => _lastError;

  DateTime? _lastSyncAt;

  DateTime? get lastSyncAt => _lastSyncAt;

  /// 待上传的操作队列（离线时暂存）。
  final List<Map<String, dynamic>> _pendingOps = [];

  int get pendingCount => _pendingOps.length;

  Timer? _debounce;

  bool get isEnabled => NasClient.instance.isEnabled;

  /// 启动时调用：设置页启用后，做一次全量对齐。
  Future<void> start() async {
    if (!isEnabled) return;
    await syncNow(full: true);
  }

  /// 记录一条收藏新增。
  ///
  /// 注意：这里只入队，真正上传由 [flush] 批量完成，
  /// 避免用户连续收藏时产生大量小请求。
  void onFavoriteAdded(FavoriteItem item) {
    if (!isEnabled) return;
    _pendingOps.add({
      'type': 'fav.add',
      'payload': {
        'folder': _folderOf(item) ?? 'Favorites',
        'source': item.sourceKey,
        'comicId': item.id,
        'title': item.name,
        'author': item.author,
        'cover': item.coverPath,
        'tags': item.tags,
        'sourceType': item.type.value,
        // 上报权威的 hashCode，服务端据此建立长期映射，避免依赖算法
        'sourceTypeHint': item.type.value,
      },
    });
    _scheduleFlush();
  }

  void onFavoriteRemoved(String folder, FavoriteItem item) {
    if (!isEnabled) return;
    _pendingOps.add({
      'type': 'fav.remove',
      'payload': {
        'folder': folder,
        'source': item.sourceKey,
        'comicId': item.id,
        'sourceType': item.type.value,
      },
    });
    _scheduleFlush();
  }

  void onFavoriteMoved(String fromFolder, String toFolder, FavoriteItem item) {
    if (!isEnabled) return;
    _pendingOps.add({
      'type': 'fav.move',
      'payload': {
        'fromFolder': fromFolder,
        'toFolder': toFolder,
        'source': item.sourceKey,
        'comicId': item.id,
        'title': item.name,
        'author': item.author,
        'cover': item.coverPath,
        'tags': item.tags,
        'sourceType': item.type.value,
      },
    });
    _scheduleFlush();
  }

  /// 记录一条阅读进度。
  void onHistoryUpdated(History h) {
    if (!isEnabled) return;
    _pendingOps.add({
      'type': 'history.update',
      'payload': {
        'source': h.type.sourceKey,
        'comicId': h.id,
        'title': h.title,
        'cover': h.cover,
        'sourceType': h.type.value,
        'ep': h.ep,
        'page': h.page,
        'group': h.group,
        'readEpisodes': h.readEpisode,
        'maxPage': h.maxPage,
        'at': h.time.millisecondsSinceEpoch,
      },
    });
    _scheduleFlush();
  }

  /// 记录源私有数据变更（登录态/token）。
  void onSourceDataChanged(String sourceKey) {
    if (!isEnabled) return;
    var source = ComicSource.find(sourceKey);
    if (source == null) return;
    _pendingOps.add({
      'type': 'source.data',
      'payload': {
        'source': sourceKey,
        'data': Map<String, dynamic>.from(source.data),
      },
    });
    _scheduleFlush();
  }

  String? _folderOf(FavoriteItem item) {
    // 查找该漫画所在的本地收藏夹
    final folders = LocalFavoritesManager().find(item.id, item.type);
    return folders.isEmpty ? null : folders.first;
  }

  void _scheduleFlush() {
    _debounce?.cancel();
    // 500ms 合并窗口：用户连续操作（如批量收藏）只发一次
    _debounce = Timer(const Duration(milliseconds: 500), () {
      flush();
    });
  }

  /// 把待上传操作推送到服务端。
  Future<void> flush() async {
    if (!isEnabled || _pendingOps.isEmpty) return;
    final client = NasClient.instance;
    if (!await client.probe()) return;

    // 取快照并清空（失败时回滚）
    final batch = List<Map<String, dynamic>>.from(_pendingOps);
    _pendingOps.clear();

    try {
      var dio = AppDio(BaseOptions(
        baseUrl: client.config.baseUrl,
        headers: {
          if (client.config.token.isNotEmpty)
            'Authorization': 'Bearer ${client.config.token}',
          'X-Client-Id': client.config.clientId,
          'Content-Type': 'application/json',
        },
        validateStatus: (s) => s != null,
      ));
      var res = await dio.post('/api/v1/sync/push', data: {
        'clientId': client.config.clientId,
        'deviceName': _deviceName(),
        'lamport': DateTime.now().millisecondsSinceEpoch,
        'ops': _dedupe(batch),
      });
      if (res.statusCode == 200 && res.data is Map) {
        var cursor = (res.data['cursor'] as num?)?.toInt();
        if (cursor != null) {
          await client.updateCursor(cursor);
        }
        _lastError = null;
      } else {
        _lastError = 'push failed: ${res.statusCode}';
        // 失败则放回队列，下次重试
        _pendingOps.insertAll(0, batch);
      }
    } catch (e) {
      _lastError = e.toString();
      _pendingOps.insertAll(0, batch);
      Log.error('NAS Sync', 'flush failed: $e');
    }
    notifyListeners();
  }

  /// 合并同一对象上的重复操作，只保留最后一条。
  ///
  /// 场景：用户快速翻页会产生大量 history.update，
  /// 只上传最后一个即可，显著减少带宽。
  List<Map<String, dynamic>> _dedupe(List<Map<String, dynamic>> ops) {
    final lastIndex = <String, int>{};
    for (var i = 0; i < ops.length; i++) {
      var key = _opKey(ops[i]);
      if (key != null) lastIndex[key] = i;
    }
    final out = <Map<String, dynamic>>[];
    for (var i = 0; i < ops.length; i++) {
      var key = _opKey(ops[i]);
      if (key != null && lastIndex[key] != i) {
        continue; // 被后续同名操作覆盖
      }
      out.add(ops[i]);
    }
    return out;
  }

  String? _opKey(Map<String, dynamic> op) {
    var type = op['type']?.toString() ?? '';
    var p = op['payload'];
    if (p is! Map) return null;
    // 收藏增删是对立操作，不能互相覆盖，只对"同类"去重
    if (type == 'history.update') {
      return '$type|${p['source']}|${p['comicId']}';
    }
    if (type == 'source.data') {
      return '$type|${p['source']}';
    }
    return null;
  }

  /// 全量同步：推送本地待发操作 + 拉取服务端变更。
  Future<void> syncNow({bool full = false}) async {
    if (!isEnabled || _isSyncing) return;
    final client = NasClient.instance;
    if (!await client.probe()) return;

    _isSyncing = true;
    _lastError = null;
    notifyListeners();

    try {
      await flush();

      if (full) {
        // 首次连接/游标丢失：拉快照重建本地状态
        await _pullSnapshot();
      } else {
        await _pullIncremental();
      }
      _lastSyncAt = DateTime.now();
    } catch (e) {
      _lastError = e.toString();
      Log.error('NAS Sync', 'sync failed: $e');
    } finally {
      _isSyncing = false;
      notifyListeners();
    }
  }

  /// 增量拉取：从记录的游标开始，拿其他设备的操作。
  Future<void> _pullIncremental() async {
    final client = NasClient.instance;
    var dio = _dio(client);
    if (dio == null) return;

    var cursor = client.config.cursor;
    for (var round = 0; round < 20; round++) {
      var res = await dio.get('/api/v1/sync/pull', queryParameters: {
        'cursor': cursor,
        'clientId': client.config.clientId,
        'limit': 500,
      });
      if (res.statusCode != 200 || res.data is! Map) return;
      var data = Map<String, dynamic>.from(res.data);

      if (data['resyncRequired'] == true) {
        await _pullSnapshot();
        return;
      }

      var ops = (data['ops'] as List?) ?? [];
      if (ops.isEmpty) {
        cursor = (data['cursor'] as num?)?.toInt() ?? cursor;
        break;
      }
      for (var op in ops) {
        _applyRemoteOp(Map<String, dynamic>.from(op as Map));
      }
      cursor = (data['cursor'] as num?)?.toInt() ?? cursor;
      await client.updateCursor(cursor);
      if (data['hasMore'] != true) break;
    }
  }

  /// 拉取完整快照（首次接入或游标过期时）。
  Future<void> _pullSnapshot() async {
    final client = NasClient.instance;
    var dio = _dio(client);
    if (dio == null) return;

    var res = await dio.get('/api/v1/sync/snapshot');
    if (res.statusCode != 200 || res.data is! Map) return;
    var snap = Map<String, dynamic>.from(res.data);

    // 收藏：按文件夹同步
    var favs = snap['favorites'];
    if (favs is Map) {
      favs.forEach((folder, items) {
        if (items is! List) return;
        for (var it in items) {
          if (it is! Map) continue;
          _applyRemoteFavorite(folder.toString(), Map<String, dynamic>.from(it));
        }
      });
    }
    var cursor = (snap['cursor'] as num?)?.toInt();
    if (cursor != null) await client.updateCursor(cursor);
  }

  Dio? _dio(NasClient client) {
    if (!client.isEnabled) return null;
    return AppDio(BaseOptions(
      baseUrl: client.config.baseUrl,
      headers: {
        if (client.config.token.isNotEmpty)
          'Authorization': 'Bearer ${client.config.token}',
        'X-Client-Id': client.config.clientId,
      },
      validateStatus: (s) => s != null,
    ));
  }

  /// 把远端操作应用到本地状态。
  ///
  /// 关键约束：只做"幂等"变更，避免与监听器形成回环
  /// （应用时不应再次触发上传）。
  void _applyRemoteOp(Map<String, dynamic> op) {
    var type = op['type']?.toString() ?? '';
    var payload = op['payload'];
    if (payload is! Map) return;
    var p = Map<String, dynamic>.from(payload);

    switch (type) {
      case 'fav.add':
        _applyRemoteFavorite(p['folder']?.toString() ?? 'Favorites', p);
        break;
      case 'fav.remove':
        _removeLocalFavorite(
          p['folder']?.toString() ?? '',
          p['source']?.toString() ?? '',
          p['comicId']?.toString() ?? '',
        );
        break;
      case 'fav.move':
        _removeLocalFavorite(
          p['fromFolder']?.toString() ?? '',
          p['source']?.toString() ?? '',
          p['comicId']?.toString() ?? '',
        );
        _applyRemoteFavorite(p['toFolder']?.toString() ?? 'Favorites', p);
        break;
      case 'history.update':
        _applyRemoteHistory(p);
        break;
      // source.data / setting.update 在多数场景下只对服务端有意义
      // （服务端需要它来跑下载），本机可忽略，避免覆盖当前登录态。
    }
  }

  void _applyRemoteFavorite(String folder, Map<String, dynamic> p) {
    try {
      var sourceKey = p['source']?.toString() ?? '';
      var comicId = p['comicId']?.toString() ?? '';
      if (sourceKey.isEmpty || comicId.isEmpty) return;
      var source = ComicSource.find(sourceKey);
      if (source == null) return; // 源未安装，跳过

      if (!LocalFavoritesManager().existsFolder(folder)) {
        LocalFavoritesManager().createFolder(folder, true);
      }
      var tags = (p['tags'] as List?)?.map((e) => '$e').toList() ?? <String>[];
      var item = FavoriteItem(
        id: comicId,
        name: p['title']?.toString() ?? comicId,
        coverPath: p['cover']?.toString() ?? '',
        author: p['author']?.toString() ?? '',
        type: ComicType(sourceKey.hashCode),
        tags: tags,
      );
      LocalFavoritesManager().addComic(folder, item, null, null);
    } catch (e) {
      Log.error('NAS Sync', 'apply remote favorite failed: $e');
    }
  }

  void _removeLocalFavorite(String folder, String sourceKey, String comicId) {
    try {
      if (folder.isEmpty) return;
      if (!LocalFavoritesManager().existsFolder(folder)) return;
      var type = ComicType(sourceKey.hashCode);
      LocalFavoritesManager().deleteComicWithId(folder, comicId, type);
    } catch (e) {
      Log.error('NAS Sync', 'remove local favorite failed: $e');
    }
  }

  void _applyRemoteHistory(Map<String, dynamic> p) {
    try {
      var sourceKey = p['source']?.toString() ?? '';
      if (ComicSource.find(sourceKey) == null) return;
      var type = ComicType(sourceKey.hashCode);
      var comicId = p['comicId']?.toString() ?? '';
      var existing = HistoryManager().find(comicId, type);
      var remoteTime = (p['at'] as num?)?.toInt() ?? 0;
      // LWW：远端更新时间更旧就不覆盖本地
      if (existing != null &&
          existing.time.millisecondsSinceEpoch >= remoteTime) {
        return;
      }
      // History.fromMap 不解析 group，因此单独构造后赋值
      var history = History.fromMap({
        'type': type.value,
        'id': comicId,
        'title': p['title']?.toString() ?? '',
        'subtitle': '',
        'cover': p['cover']?.toString() ?? '',
        'ep': (p['ep'] as num?)?.toInt() ?? 0,
        'page': (p['page'] as num?)?.toInt() ?? 0,
        'max_page': (p['maxPage'] as num?)?.toInt(),
        'time': remoteTime,
        'readEpisode':
            (p['readEpisodes'] as List?)?.map((e) => '$e').toList() ?? const [],
      });
      var group = (p['group'] as num?)?.toInt() ?? 0;
      if (group > 0) {
        history.group = group;
      }
      HistoryManager().addHistory(history);
    } catch (e) {
      Log.error('NAS Sync', 'apply remote history failed: $e');
    }
  }

  String _deviceName() {
    try {
      return Platform.operatingSystem;
    } catch (_) {
      return 'unknown';
    }
  }
  /// 停止并清理。
  void disposeSync() {
    _debounce?.cancel();
    _debounce = null;
  }
}
