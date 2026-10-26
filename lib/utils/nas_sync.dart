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
import 'package:venera/utils/nas_settings_sync.dart';

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
    final folder = _folderOf(item) ?? 'Favorites';
    // 带上显示顺序，让其他设备落库后排列一致
    final order = LocalFavoritesManager().displayOrders(folder)[item.id];
    _pendingOps.add({
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
        if (order != null) 'order': order,
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
    final data = Map<String, dynamic>.from(source.data);
    // 空 data 不推 data op（服务端有"拒绝空值覆盖"的保护，推了也是白推），
    // 但 **Cookie 仍然要推**：不需要账号的源只靠 Cookie 维持登录态。
    if (data.isNotEmpty) {
      _pendingOps.add({
        'type': 'source.data',
        'payload': {'source': sourceKey, 'data': data},
      });
    }
    _enqueueCookies(sourceKey, data);
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
        // 首次连接 / 游标丢失时的顺序**至关重要**：先拉后推。
        //
        // 1) 先拉快照：接收其他设备（手机）已有的收藏、历史、设置。
        //    新装的 App 本地是"一堆默认值"（readerMode=galleryLeftToRight、
        //    explore_pages=[] 等）。若先推，这些默认值会把手机上
        //    真实配置覆盖掉——实测 pad 首次连接就把 readerMode 从
        //    "page" 改成了 "galleryLeftToRight"，手机跟着也变了。
        //
        // 2) 再推本地：把"这台设备独有、服务端还没有"的数据补上去
        //    （典型场景：手机第一次连 NAS，服务端是空的）。
        //
        // 两步都是幂等的：收藏是集合语义、历史是 LWW、设置是覆盖。
        await _pullSnapshot();
        await _bootstrapPush();
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
        // 顺序必须一起传：客户端列表按 display_order 升序展示，
        // 接收端如果拿不到这个值，会按"新条目插到开头"处理，
        // 结果整批收藏的顺序被完全颠倒（实测现象）。
        final orders = LocalFavoritesManager().displayOrders(folder);
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
              if (orders[item.id] != null) 'order': orders[item.id],
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
        final plain = Map<String, dynamic>.from(data);
        // 注意：不能因为 data 为空就 continue。
        //
        // 很多源**根本不需要账号密码**（nhentai 就是典型：data 文件是空的），
        // 但登录态仍然在 Cookie 里。早先的写法 `if (data.isEmpty) continue;`
        // 会把这类源连同它的 Cookie 一起跳过，表现为"服务端一条 nhentai
        // cookie 都没有"，登录态自然传不过去。
        if (plain.isNotEmpty) {
          ops.add({
            'type': 'source.data',
            'payload': {'source': source.key, 'data': plain},
          });
        }
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

    // ---- 界面结构类设置（探索页 / 分类页 / 搜索源 / 阅读器偏好 ...）----
    // 不推这些，新设备上"收藏都有了但探索页是空的"。
    try {
      ops.addAll(NasSettingsSync.buildOps());
    } catch (e) {
      Log.error('NAS Sync', 'bootstrap: read settings failed: $e');
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

    // ---- 阅读历史 ----
    // 早期实现只统计了 histories 的数量、却从没调用 _applyRemoteHistory，
    // 于是"收藏同步了、历史一条都没有"——数据在快照里，就是没人处理。
    var histories = snap['histories'];
    var histApplied = 0;
    var histFailed = 0;
    if (histories is List) {
      for (final h in histories) {
        if (h is! Map) continue;
        try {
          _applyRemoteHistory(Map<String, dynamic>.from(h));
          histApplied++;
        } catch (e) {
          histFailed++;
        }
      }
    }

    Log.info(
      'NAS Sync',
      'snapshot applied: favorites=$applied failed=$failed '
      'histories=$histApplied/${(histories as List?)?.length ?? 0} '
      'histFailed=$histFailed'
      '${_firstApplyError != null ? ' | first error: $_firstApplyError' : ''}',
    );

    var cursor = (snap['cursor'] as num?)?.toInt();
    if (cursor != null) await client.updateCursor(cursor);

    // ---- 界面结构类设置 ----
    // 「探索」页显示"没有探索页面"、「分类」页显示"没有分类页面"，
    // 根因是这两页由 appdata.settings 的列表驱动，与收藏无关。
    // 早期版本完全没同步设置，所以数据到了、页面还是空的。
    //
    // 必须 await：full 同步是"先拉后推"，若设置还没落地就开始推，
    // 推上去的会是本机的出厂默认值，把别的设备的配置覆盖掉。
    await _applyRemoteSettings(snap['settings']);
  }

  /// 应用快照里的设置项。
  Future<void> _applyRemoteSettings(dynamic raw) async {
    if (raw is! Map) return;
    final remote = <String, dynamic>{};
    for (final e in raw.entries) {
      remote[e.key.toString()] = e.value;
    }
    if (remote.isEmpty) return;

    try {
      final n = await NasSettingsSync.apply(remote);
      if (n > 0) {
        Log.info('NAS Sync', 'applied $n settings from NAS');
      }
    } catch (e) {
      Log.error('NAS Sync', 'apply settings failed: $e');
    }
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
      // 用远端给的 display_order 落库，保持与源设备一致的排列。
      //
      // 传 null 会让 addComic 走"插到开头"（minValue-1）的默认分支：
      // 逐条应用时每一条都跑到最前面，最终整批顺序被完全颠倒——
      // 这正是"pad 上收藏顺序和手机反着"的原因。
      final order = (p['order'] ?? p['displayOrder']) as num?;
      LocalFavoritesManager().addComic(
        folder,
        item,
        order?.toInt(),
        null,
      );
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
      var remoteTime = _timeOf(p);
      // LWW：远端更新时间更旧就不覆盖本地。
      //
      // remoteTime == 0 表示对方没给时间（旧数据/字段缺失），
      // 此时不能套用 LWW —— 否则"本地任意记录 >= 0"恒成立，
      // 会把真实进度当成"更旧的"丢掉，正是历史同步失效的表现。
      if (existing != null &&
          remoteTime > 0 &&
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

  /// 从远端历史 payload 里解析时间（毫秒）。
  ///
  /// 三种形态都要认，因为两端字段名不一致且历史数据还混着旧格式：
  ///   at         增量 op / 新快照（int 毫秒）
  ///   updatedAt  旧快照（RFC3339 字符串）
  ///   time       某些历史版本用的字段
  ///
  /// 早期实现只读 `at`，拿到 0 后 LWW 判定"本地更新"，
  /// 于是所有历史记录被静默跳过 —— 收藏同步了、历史一条都没有。
  static int _timeOf(Map<String, dynamic> p) {
    final raw = p['at'] ?? p['updatedAt'] ?? p['time'];
    if (raw == null) return 0;
    if (raw is num) return raw.toInt();
    final s = raw.toString();
    if (s.isEmpty) return 0;
    final asInt = int.tryParse(s);
    if (asInt != null) return asInt;
    final dt = DateTime.tryParse(s);
    return dt?.millisecondsSinceEpoch ?? 0;
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
