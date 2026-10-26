import 'dart:async';
import 'dart:io' show HttpClient, Platform;

import 'package:dio/dio.dart';
import 'package:dio/io.dart';
import 'package:flutter/foundation.dart';

import 'package:venera/foundation/comic_source/comic_source.dart';
import 'package:venera/foundation/comic_type.dart';
import 'package:venera/foundation/favorites.dart';
import 'package:venera/foundation/history.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/utils/nas_cookie_export.dart';

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

  /// 首次应用远端收藏失败的原因（诊断用，成功后清空）。
  String? _firstApplyError;

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
    _enqueueCookies(sourceKey, source.data);
    _scheduleFlush();
  }

  /// 把该源当前的 Cookie 一起推给服务端。
  ///
  /// 登录态常常**只存在于 Cookie 里**（ehentai 尤其明显：igneous +
  /// ipb_member_id + ipb_pass_hash）。不同步 Cookie 的话，服务端拿到的
  /// 账号数据是空壳，请求会被 302 到登录页，表现为 404/无权限。
  void _enqueueCookies(String sourceKey, Map<String, dynamic> data) {
    try {
      final cookies =
          NasCookieExport.exportForSource(sourceKey, Map.of(data));
      if (cookies.isEmpty) return;
      _pendingOps.add({
        'type': 'source.cookie',
        'payload': {'source': sourceKey, 'cookies': cookies},
      });
    } catch (e) {
      Log.error('NAS Sync', 'export cookies for $sourceKey failed: $e');
    }
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
      var dio = _dio(client);
      if (dio == null) {
        _pendingOps.insertAll(0, batch);
        return;
      }
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
        // 首次连接 / 游标丢失：
        //   1. 先把本地已有的收藏与历史**全量上推**（否则服务端永远是空的，
        //      因为它只见到"变更"，而用户的历史数据在连 NAS 之前就存在了）
        //   2. 再拉快照，接收其他设备的既有数据
        await _bootstrapPush();
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

  /// 首次连接时把本地既有数据全量上推到服务端。
  ///
  /// 为什么需要这一步：增量同步只跟踪"连接之后的变更"。用户的历史收藏、
  /// 阅读进度早在连接 NAS 之前就已存在，服务端永远看不到它们，
  /// 结果就是"连上了但服务端是空的、也不会去下载收藏"。
  ///
  /// 分批推送（每批 200 条）避免单个请求过大；服务端侧对重复 op 幂等，
  /// 因此重复执行安全。
  Future<void> _bootstrapPush() async {
    final client = NasClient.instance;
    var dio = _dio(client);
    if (dio == null) return;

    final ops = <Map<String, dynamic>>[];

    // ---- 收藏 ----
    try {
      final folders = LocalFavoritesManager().folderNames;
      for (final folder in folders) {
        final comics = LocalFavoritesManager().getFolderComics(folder);
        for (final item in comics) {
          ops.add({
            'type': 'fav.add',
            'payload': {
              'folder': folder,
              'source': item.sourceKey,
              'comicId': item.id,
              'title': item.name,
              'author': item.author,
              'cover': item.coverPath,
              'tags': item.tags,
              'sourceType': item.type.value,
            },
          });
        }
      }
    } catch (e) {
      Log.error('NAS Sync', 'bootstrap: read favorites failed: $e');
    }

    // ---- 阅读历史 ----
    try {
      final histories = HistoryManager().getAll();
      for (final h in histories) {
        ops.add({
          'type': 'history.update',
          'payload': {
            'source': h.type.sourceKey,
            'comicId': h.id,
            'title': h.title,
            'cover': h.cover,
            'sourceType': h.type.value,
            'ep': h.ep,
            'page': h.page,
            'group': h.group ?? 0,
            'readEpisodes': h.readEpisode.toList(),
            'maxPage': h.maxPage,
            'at': h.time.millisecondsSinceEpoch,
          },
        });
      }
    } catch (e) {
      Log.error('NAS Sync', 'bootstrap: read history failed: $e');
    }

    // ---- 源配置（账号、密码、baseUrl 等自定义项）----
    // 首次连接必须把 App 里各站点的私有配置推给服务端，否则服务端拿着
    // 空配置去下载收藏，会得到 "invalid url: null/comics/..." 这类报错
    // （picacg 的 apiUrl 为空就是这样）。
    try {
      for (final source in ComicSourceManager().all()) {
        final data = source.data;
        if (data.isEmpty) continue;
        final plain = Map<String, dynamic>.from(data);
        ops.add({
          'type': 'source.data',
          'payload': {'source': source.key, 'data': plain},
        });
        // 登录态常常只存在于 Cookie 里，必须一起推
        final cookies = NasCookieExport.exportForSource(source.key, plain);
        if (cookies.isNotEmpty) {
          ops.add({
            'type': 'source.cookie',
            'payload': {'source': source.key, 'cookies': cookies},
          });
        }
      }
    } catch (e) {
      Log.error('NAS Sync', 'bootstrap: read source configs failed: $e');
    }

    if (ops.isEmpty) return;
    Log.info('NAS Sync', 'bootstrap: pushing ${ops.length} existing records');

    const batchSize = 200;
    for (var i = 0; i < ops.length; i += batchSize) {
      final batch = ops.sublist(i, (i + batchSize).clamp(0, ops.length));
      try {
        var res = await dio.post('/api/v1/sync/push', data: {
          'clientId': client.config.clientId,
          'deviceName': _deviceName(),
          'lamport': DateTime.now().millisecondsSinceEpoch,
          'ops': batch,
        });
        if (res.statusCode == 200 && res.data is Map) {
          var cursor = (res.data['cursor'] as num?)?.toInt();
          if (cursor != null) await client.updateCursor(cursor);
        } else {
          Log.error('NAS Sync', 'bootstrap push failed: ${res.statusCode}');
        }
      } catch (e) {
        Log.error('NAS Sync', 'bootstrap push error: $e');
        return;
      }
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
  ///
  /// 这是「新设备装上就能看到收藏」的唯一入口，因此每一步都要能自证：
  /// 失败不再是静默的（历史 bug：连接显示成功、收藏一条都没有，
  /// 界面上完全看不出是哪一步断了）。
  Future<void> _pullSnapshot() async {
    final client = NasClient.instance;
    var dio = _dio(client);
    if (dio == null) return;

    var res = await dio.get('/api/v1/sync/snapshot');
    if (res.statusCode != 200 || res.data is! Map) {
      _lastError = 'snapshot failed: HTTP ${res.statusCode}';
      Log.error('NAS Sync', _lastError!);
      return;
    }
    var snap = Map<String, dynamic>.from(res.data);

    var applied = 0;
    var failed = 0;
    _firstApplyError = null;

    // 收藏：按文件夹同步
    var favs = snap['favorites'];
    if (favs is Map) {
      favs.forEach((folder, items) {
        if (items is! List) return;
        final folderName = folder.toString();
        for (var it in items) {
          if (it is! Map) continue;
          final ok = _applyRemoteFavorite(
            folderName,
            Map<String, dynamic>.from(it),
          );
          if (ok) {
            applied++;
          } else {
            failed++;
          }
        }
      });
    }

    Log.info(
      'NAS Sync',
      'snapshot applied: favorites=$applied failed=$failed '
      'histories=${(snap['histories'] as List?)?.length ?? 0}'
      '${_firstApplyError != null ? ' | first error: $_firstApplyError' : ''}',
    );

    var cursor = (snap['cursor'] as num?)?.toInt();
    if (cursor != null) await client.updateCursor(cursor);
  }

  Dio? _dio(NasClient client) {
    if (!client.isEnabled) return null;
    // 与 NasClient 保持一致：用纯 Dart HTTP，不用 RHttpAdapter
    // （后者依赖 flutter_rust_bridge，未初始化时所有请求都会抛异常）
    final dio = Dio(BaseOptions(
      baseUrl: client.config.baseUrl,
      connectTimeout: const Duration(seconds: 5),
      receiveTimeout: const Duration(seconds: 30),
      headers: {
        if (client.config.token.isNotEmpty)
          'Authorization': 'Bearer ${client.config.token}',
        'X-Client-Id': client.config.clientId,
      },
      validateStatus: (s) => s != null,
    ));
    dio.httpClientAdapter = IOHttpClientAdapter(
      createHttpClient: () => HttpClient()
        ..connectionTimeout = const Duration(seconds: 5)
        // 局域网 NAS 必须直连，不能走用户配置的代理
        ..findProxy = ((uri) => 'DIRECT'),
    );
    return dio;
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

  /// 应用一条远端收藏。返回是否成功写入本地。
  bool _applyRemoteFavorite(String folder, Map<String, dynamic> p) {
    try {
      // 字段名兼容：服务端的 Favorite 结构把漫画 id 序列化成 `id`
      // （见 model.Comic 的 json tag），而操作日志 payload 里叫 `comicId`。
      // 快照走前者、增量走后者，只读一个必然有一边全是空值——
      // 这正是"449 条收藏全部失败但没有任何异常"的原因：
      // 代码在早期 return false，而不是抛错。
      var sourceKey = (p['source'] ?? p['sourceKey'])?.toString() ?? '';
      var comicId =
          (p['comicId'] ?? p['id'])?.toString() ?? '';
      if (sourceKey.isEmpty || comicId.isEmpty) {
        if (_firstApplyError == null) {
          _firstApplyError =
              'missing field: source=$sourceKey comicId=$comicId keys=${p.keys.join(",")}';
        }
        return false;
      }

      // 关键：**不要**因为"源未安装"就丢弃收藏。
      //
      // 这正是"新设备登录后收藏空白"的根因：平板刚装好 App、还没导入任何
      // 漫画源，`ComicSource.find()` 一定返回 null。若此时跳过，
      // 服务端明明有 993 条收藏，客户端却一条也不落地——
      // 而收藏项自身已经带齐了 id/标题/封面/作者，渲染根本不需要源。
      //
      // 因此优先用 op payload 里服务端给的 sourceType（与手机端一致）；
      // 拿不到时再用 sourceKey.hashCode 兜底（Dart 侧算法稳定）。
      //
      // 代价：源装好之后 `ComicType.value` 才能解析出真实源对象。
      // 那之前收藏按"未知源"展示，但数据不会丢，装源后即刻恢复正常。
      final typeHint = (p['sourceType'] as num?)?.toInt() ??
          (p['sourceTypeHint'] as num?)?.toInt();

      // 建文件夹时必须传 renameWhenInvalidName=false。
      //
      // 这个参数看着像"名字非法就改名"，实际语义是"**已存在就改名**"
      // （见 LocalFavoritesManager.createFolder：existsFolder(name) 时
      // 追加数字后缀）。同步场景下这会造成连锁失败：
      //   第一次 Picacg 建好了 → 第二次 createFolder('Picacg', true)
      //   把它改成 'Picacg0' → 紧接着 addComic('Picacg', …) 抛
      //   "Folder does not exists" → 该条收藏丢失。
      //
      // 实测后果：449 条收藏一条都没落地（favorites=0 failed=449）。
      if (!LocalFavoritesManager().existsFolder(folder)) {
        LocalFavoritesManager().createFolder(folder);
      }
      var tags = (p['tags'] as List?)?.map((e) => '$e').toList() ?? <String>[];
      var item = FavoriteItem(
        id: comicId,
        name: p['title']?.toString() ?? comicId,
        coverPath: p['cover']?.toString() ?? '',
        author: p['author']?.toString() ?? '',
        type: typeHint != null && typeHint != 0
            ? ComicType(typeHint)
            : ComicType(sourceKey.hashCode),
        tags: tags,
      );
      LocalFavoritesManager().addComic(folder, item, null, null);
      return true;
    } catch (e, st) {
      // 记录**第一条**失败的具体原因：449 条全部失败时，
      // 一条完整堆栈比 449 条同样的摘要有用得多。
      if (_firstApplyError == null) {
        _firstApplyError = '$e\n${st.toString().split('\n').take(4).join('\n')}';
        Log.error('NAS Sync', 'first favorite apply failure: $_firstApplyError');
      }
      return false;
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
      var sourceKey = (p['source'] ?? p['sourceKey'])?.toString() ?? '';
      if (sourceKey.isEmpty) return;
      // 同 _applyRemoteFavorite：不要因为源未安装就丢弃阅读进度。
      final typeHint = (p['sourceType'] as num?)?.toInt();
      var type = typeHint != null && typeHint != 0
          ? ComicType(typeHint)
          : ComicType(sourceKey.hashCode);
      // 字段名兼容：快照用 `id`，增量 payload 用 `comicId`。
      var comicId = (p['comicId'] ?? p['id'])?.toString() ?? '';
      if (comicId.isEmpty) return;
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
