// 注意：这里刻意使用标准 Dio + 纯 Dart 的 IOHttpClientAdapter，
// 而不是项目里的 AppDio。
//
// 原因：AppDio 在构造时硬编码 `httpClientAdapter = RHttpAdapter()`，
// 依赖 flutter_rust_bridge（Rust 实现）。若 Rust 侧未初始化完成
// （例如冷启动早期、或某些 ABI 上初始化失败），所有请求都会抛
//   Bad state: flutter_rust_bridge has not been initialized
// NAS 是纯 HTTP/JSON，用系统 HTTP 栈更可靠，也避免和 Rust 生命周期耦合。
import 'dart:io' show HttpClient, Platform;
import 'dart:typed_data' show Uint8List;

import 'package:dio/dio.dart';
import 'package:dio/io.dart';

import 'package:venera/foundation/appdata.dart';
import 'package:venera/foundation/log.dart';

/// NAS 服务端配置。
class NasConfig {
  final String baseUrl;
  final String token;

  /// 客户端标识，用于 W2 同步的回显抑制与多设备区分。
  final String clientId;

  /// 上一次成功同步到的服务端游标（op id）。
  final int cursor;

  const NasConfig({
    required this.baseUrl,
    required this.token,
    required this.clientId,
    this.cursor = 0,
  });

  NasConfig copyWith({String? baseUrl, String? token, String? clientId, int? cursor}) {
    return NasConfig(
      baseUrl: baseUrl ?? this.baseUrl,
      token: token ?? this.token,
      clientId: clientId ?? this.clientId,
      cursor: cursor ?? this.cursor,
    );
  }

  /// baseUrl 归一化：去掉末尾斜杠，补全 scheme。
  static String normalize(String input) {
    var s = input.trim();
    if (s.isEmpty) return s;
    if (!s.startsWith('http://') && !s.startsWith('https://')) {
      s = 'http://$s';
    }
    while (s.endsWith('/')) {
      s = s.substring(0, s.length - 1);
    }
    return s;
  }

  Map<String, dynamic> toJson() => {
        'baseUrl': baseUrl,
        'token': token,
        'clientId': clientId,
        'cursor': cursor,
      };

  static NasConfig fromJson(Map<String, dynamic> json) => NasConfig(
        baseUrl: json['baseUrl'] ?? '',
        token: json['token'] ?? '',
        clientId: json['clientId'] ?? '',
        cursor: json['cursor'] ?? 0,
      );

  bool get isValid => baseUrl.isNotEmpty;
}

/// NAS 服务端返回的一章缓存信息。
class NasChapter {
  final List<String> pages;
  final String? cover;
  final int bytes;

  const NasChapter({
    required this.pages,
    this.cover,
    this.bytes = 0,
  });

  bool get isAvailable => pages.isNotEmpty;
}

/// NAS 服务端返回的一部漫画缓存概览。
class NasLibraryItem {
  final String source;
  final String comicId;
  final String title;
  final int chapters;
  final int bytes;

  const NasLibraryItem({
    required this.source,
    required this.comicId,
    required this.title,
    required this.chapters,
    required this.bytes,
  });
}

/// NAS 在线状态。
enum NasStatus {
  /// 未配置
  disabled,

  /// 已配置但当前不可达
  offline,

  /// 在线
  online,
}

/// 详情页的 NAS 兜底数据。
///
/// 当源站不可达时用它渲染"降级版详情页"：
/// 有标题、封面、标签、已缓存章节，用户可以直接读已缓存的内容。
class NasComicFallback {
  final String sourceKey;
  final String comicId;
  final String title;
  final String subtitle;
  final String cover;
  final List<String> tags;

  /// 作者 / 上传者 / 简介。
  ///
  /// 服务端下载时会把这些一并落库（comics 表的 author/uploader/description）。
  /// 早期只存 title+cover+tags，导致离线兜底详情页缺作者、缺简介，
  /// 与在线页明显不一致（用户实测反馈"缓存的详情信息不匹配"）。
  final String author;
  final String uploader;
  final String description;

  /// 已缓存章节：章节 id / 标题 / 页数。
  final List<NasFallbackChapter> chapters;

  /// 与 [chapters] 一一对应的"是否已缓存"标记。
  ///
  /// 只有在服务端返回了**源端全量章节表**时才非空：那时 [chapters]
  /// 里的未缓存章节也会列出来（保证下标与源端一致），但不能点。
  final List<bool> cachedFlags;

  /// 是否拥有源端完整章节表（决定渲染时能不能灰掉未缓存章节）。
  bool get hasFullList => cachedFlags.isNotEmpty;

  const NasComicFallback({
    required this.sourceKey,
    required this.comicId,
    required this.title,
    this.subtitle = '',
    this.cover = '',
    this.tags = const [],
    this.author = '',
    this.uploader = '',
    this.description = '',
    this.chapters = const [],
    this.cachedFlags = const [],
  });

  bool get isEmpty => chapters.isEmpty;

  static NasComicFallback fromJson(Map<String, dynamic> m) {
    final chs = <NasFallbackChapter>[];
    final flags = <bool>[];

    // 优先用源端全量章节表：客户端用 history.ep 当**下标**找章节，
    // 只给已缓存章节会让下标整体前移，打开错误的章节。
    final full = m['fullChapters'];
    if (full is List && full.isNotEmpty) {
      for (final e in full) {
        if (e is! Map) continue;
        chs.add(NasFallbackChapter(
          id: e['id']?.toString() ?? '',
          title: e['title']?.toString() ?? '',
          pages: (e['pages'] as num?)?.toInt() ?? 0,
        ));
        flags.add(e['cached'] == true);
      }
      final tagsRaw = _tagsOf(m);
      return NasComicFallback(
        sourceKey: m['source']?.toString() ?? '',
        comicId: m['comicId']?.toString() ?? '',
        title: m['title']?.toString() ?? '',
        subtitle: m['subtitle']?.toString() ?? '',
        cover: m['cover']?.toString() ?? '',
        tags: tagsRaw,
        author: m['author']?.toString() ?? '',
        uploader: m['uploader']?.toString() ?? '',
        description: m['description']?.toString() ?? '',
        chapters: chs,
        cachedFlags: flags,
      );
    }

    // 回退：老服务端只给已缓存章节
    final raw = m['chapters'];
    if (raw is List) {
      for (final e in raw) {
        if (e is! Map) continue;
        chs.add(NasFallbackChapter(
          id: e['id']?.toString() ?? '',
          title: e['title']?.toString() ?? '',
          pages: (e['pages'] as num?)?.toInt() ?? 0,
        ));
        flags.add(true);
      }
    }
    return NasComicFallback(
      sourceKey: m['source']?.toString() ?? '',
      comicId: m['comicId']?.toString() ?? '',
      title: m['title']?.toString() ?? '',
      subtitle: m['subtitle']?.toString() ?? '',
      cover: m['cover']?.toString() ?? '',
      tags: _tagsOf(m),
      author: m['author']?.toString() ?? '',
      uploader: m['uploader']?.toString() ?? '',
      description: m['description']?.toString() ?? '',
      chapters: chs,
      cachedFlags: flags,
    );
  }

  static List<String> _tagsOf(Map<String, dynamic> m) {
    final tagsRaw = m['tags'];
    final tags = <String>[];
    if (tagsRaw is List) {
      for (final t in tagsRaw) {
        final s = t?.toString() ?? '';
        if (s.isNotEmpty) tags.add(s);
      }
    }
    return tags;
  }
}

/// 兜底数据里的单个章节。
class NasFallbackChapter {
  final String id;
  final String title;
  final int pages;

  const NasFallbackChapter({
    required this.id,
    required this.title,
    this.pages = 0,
  });
}

/// 把 NAS 兜底数据转成 ComicDetails 的 JSON 形态。
///
/// 单独放在这里而不是给 ComicDetails 加构造器：ComicDetails 是源脚本
/// 契约的一部分（JSON 形状固定），不该为了一个离线兜底而改它的 API。
///
/// 注意：未缓存章节的灰化标记不放在返回的 JSON 里——ComicDetails.fromJson
/// 会丢弃不认识的键。这类 UI 信息由调用方直接从 [NasComicFallback.cachedFlags]
/// 取（见 _ComicPageState.nasFallback）。
Map<String, dynamic> nasFallbackToComicDetailsJson(
  NasComicFallback fb,
  String sourceKey,
) {
  final chapters = <String, String>{};
  for (final c in fb.chapters) {
    // 章节标题可能为空（源没给），用 id 兜底，避免列表里出现空行
    chapters[c.id] = c.title.isEmpty ? c.id : c.title;
  }
  // ComicDetails 没有 author 字段（作者一直是以 tags 里的
  // "Author:xxx" 形式展示的），所以这里把手里的作者补进标签组，
  // 保证离线页与在线页的展示位置一致。
  final tags = _groupTags(fb.tags);
  final author = fb.author.isEmpty ? _authorFromTags(fb.tags) : fb.author;
  if (author != null && author.isNotEmpty && !tags.containsKey('Author')) {
    tags['Author'] = [author];
  }

  return {
    'title': fb.title.isEmpty ? fb.comicId : fb.title,
    'subtitle': fb.subtitle,
    'cover': fb.cover,
    'description': fb.description.isEmpty ? null : fb.description,
    'tags': tags,
    'chapters': chapters,
    'thumbnails': null,
    'recommend': null,
    'sourceKey': sourceKey,
    'comicId': fb.comicId,
    'isFavorite': null,
    'subId': null,
    'isLiked': null,
    'likesCount': null,
    'commentCount': null,
    'uploader': fb.uploader.isEmpty ? null : fb.uploader,
    'uploadTime': null,
    'updateTime': null,
    'url': null,
  };
}

/// 从 `Author:xxx` / `作者:xxx` 形式的标签里取作者名。
String? _authorFromTags(List<String> tags) {
  for (final t in tags) {
    final i = t.indexOf(':');
    if (i <= 0) continue;
    final ns = t.substring(0, i).trim().toLowerCase();
    if (ns == 'author' || ns == 'artist' || ns == '作者' || ns == '作者名') {
      final v = t.substring(i + 1).trim();
      if (v.isNotEmpty) return v;
    }
  }
  return null;
}

/// 把缓存的平铺标签还原成源站的命名空间分组。
///
/// 服务端存的是源站 tags 的**平铺形态**（`Categories:長篇`、
/// `Tags:蘿莉`、`Chinese Team:xxx`…），因为它按 `ns:value` 存储。
/// 早期实现把它们整体塞进一个叫 `Tag` 的分组，于是兜底详情页显示成
/// 一长串「Tag: Categories:長篇 / Tags:蘿莉」，与源站的
/// 「作者 / 汉化组 / 分类 / 标签」分组完全对不上——用户实测
/// 《真夜中の夜子さん》就是这个现象。
///
/// 这里按第一个冒号切回 `{ns: [values]}`，与 ComicDetails 的
/// tags 结构（Map<String, List<String>>）一致，UI 自然就分组了。
/// 没有冒号的标签归到 `Tag` 兜底组，不丢数据。
Map<String, List<String>> _groupTags(List<String> raw) {
  final grouped = <String, List<String>>{};
  for (final t in raw) {
    final s = t.trim();
    if (s.isEmpty) continue;
    final i = s.indexOf(':');
    if (i <= 0 || i == s.length - 1) {
      // 无命名空间：放进通用组
      grouped.putIfAbsent('Tag', () => <String>[]).add(s);
      continue;
    }
    final ns = s.substring(0, i).trim();
    final value = s.substring(i + 1).trim();
    if (ns.isEmpty || value.isEmpty) {
      grouped.putIfAbsent('Tag', () => <String>[]).add(s);
      continue;
    }
    grouped.putIfAbsent(ns, () => <String>[]).add(value);
  }
  return grouped;
}

/// NAS 客户端：封装 W2 同步协议与缓存查询。
///
/// 设计原则：
///   - 所有网络失败都降级为"离线"，绝不阻断 App 现有流程；
///   - 同步是增量的（游标），不做整包传输；
///   - 明确定义"NAS 优先"只影响读取，不影响本地已下载内容。
class NasClient {
  NasClient._();

  static final NasClient instance = NasClient._();

  NasConfig _config = const NasConfig(baseUrl: '', token: '', clientId: '');

  NasStatus _status = NasStatus.disabled;

  NasStatus get status => _status;

  NasConfig get config => _config;

  Dio? _dio;

  /// 最近一次探测时间，避免高频轮询。
  DateTime? _lastProbe;

  /// 探测结果缓存时长。
  static const _probeInterval = Duration(seconds: 20);

  bool get isEnabled => _config.isValid;

  /// 从 appdata 载入配置。
  void load() {
    try {
      var raw = appdata.settings['nas'];
      if (raw is Map) {
        _config = NasConfig.fromJson(Map<String, dynamic>.from(raw));
      } else {
        _config = const NasConfig(baseUrl: '', token: '', clientId: '');
      }
    } catch (e) {
      Log.error('NAS', 'load config failed: $e');
    }
    _rebuildDio();
    _status = _config.isValid ? NasStatus.offline : NasStatus.disabled;
  }

  /// 保存配置并重建连接。
  Future<void> save(NasConfig config) async {
    _config = config.copyWith(baseUrl: NasConfig.normalize(config.baseUrl));
    _rebuildDio();
    appdata.settings['nas'] = _config.toJson();
    await appdata.saveData();
    await probe(force: true);
  }

  /// 清除配置。
  Future<void> clear() async {
    _config = const NasConfig(baseUrl: '', token: '', clientId: '');
    _dio = null;
    _status = NasStatus.disabled;
    appdata.settings['nas'] = null;
    await appdata.saveData();
  }

  /// 判断某个图片 URL 是否指向 NAS 服务。
  ///
  /// NAS 返回的 URL 自带 token、可直取，不能再交给源脚本的
  /// getImageLoadingConfig 处理，否则 token 会被丢掉导致 401。
  static bool isNasUrl(String url) {
    if (url.isEmpty) return false;
    final base = instance._config.baseUrl;
    if (base.isEmpty) return false;
    return url.startsWith(base);
  }

  void _rebuildDio() {    if (!_config.isValid) {
      _dio = null;
      return;
    }
    final dio = Dio(BaseOptions(
      baseUrl: _config.baseUrl,
      connectTimeout: const Duration(seconds: 5),
      receiveTimeout: const Duration(seconds: 20),
      headers: {
        if (_config.token.isNotEmpty) 'Authorization': 'Bearer ${_config.token}',
        'X-Client-Id': _config.clientId,
      },
      // 让调用方自行判断状态码，避免每次非 2xx 都抛异常
      validateStatus: (s) => s != null,
    ));
    // 纯 Dart HTTP 实现，不依赖 flutter_rust_bridge
    //
    // 关键：NAS 是局域网服务，必须**绕过**用户配置的代理。
    // App 的「设置 → 网络 → 代理 → manual」针对的是访问漫画源站点的流量；
    // 若 NAS 请求也走代理，局域网地址（如 192.168.x.x）会被代理吞掉或超时。
    // 因此这里显式设置 findProxy 为 DIRECT。
    dio.httpClientAdapter = IOHttpClientAdapter(
      createHttpClient: () {
        final client = HttpClient()
          ..connectionTimeout = const Duration(seconds: 5)
          // 强制直连，忽略系统/环境代理
          ..findProxy = ((uri) => 'DIRECT')
          ..badCertificateCallback = (cert, host, port) => false;
        return client;
      },
    );
    _dio = dio;
  }

  /// 探测服务是否可达。
  Future<bool> probe({bool force = false}) async {
    if (!isEnabled) {
      _status = NasStatus.disabled;
      return false;
    }
    if (!force &&
        _lastProbe != null &&
        DateTime.now().difference(_lastProbe!) < _probeInterval) {
      return _status == NasStatus.online;
    }
    final dio = _dio;
    if (dio == null) return false;
    try {
      var res = await dio.get('/api/v1/ping');
      bool ok = res.statusCode == 200 && res.data is Map && res.data['ok'] == true;
      _status = ok ? NasStatus.online : NasStatus.offline;
      _lastProbe = DateTime.now();
      return ok;
    } catch (e) {
      _status = NasStatus.offline;
      _lastProbe = DateTime.now();
      return false;
    }
  }

  /// 重置探测缓存（例如恢复网络后立即重试）。
  void invalidateProbe() {
    _lastProbe = null;
  }

  /// 查询某一章是否已在 NAS 上缓存。
  ///
  /// 这是"NAS 优先读取"的核心调用。返回 null 表示不可用（离线/未缓存/出错），
  /// 调用方应回退到原有网络逻辑。
  Future<NasChapter?> findChapter({
    required String sourceKey,
    required String comicId,
    required String chapterId,
  }) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.get('/api/v1/chapter', queryParameters: {
        'source': sourceKey,
        'comicId': comicId,
        'chapterId': chapterId,
      });
      if (res.statusCode != 200 || res.data is! Map) return null;
      var data = Map<String, dynamic>.from(res.data);
      if (data['cached'] != true) return null;
      var pages = (data['pages'] as List?)?.map((e) => '$e').toList() ?? [];
      if (pages.isEmpty) return null;
      _status = NasStatus.online;
      return NasChapter(
        pages: pages,
        cover: data['cover']?.toString(),
        bytes: (data['bytes'] as num?)?.toInt() ?? 0,
      );
    } catch (e) {
      _status = NasStatus.offline;
      return null;
    }
  }

  /// 查询某部漫画在 NAS 上已缓存的章节 ID 集合。
  Future<Set<String>> cachedChapters(String sourceKey, String comicId) async {
    if (!isEnabled) return {};
    final dio = _dio;
    if (dio == null) return {};
    try {
      var res = await dio.get('/api/v1/library/$sourceKey/${Uri.encodeComponent(comicId)}');
      if (res.statusCode != 200 || res.data is! Map) return {};
      var list = (res.data['chapters'] as List?) ?? [];
      return list
          .map((e) => (e as Map)['chapterId']?.toString() ?? '')
          .where((e) => e.isNotEmpty)
          .toSet();
    } catch (e) {
      return {};
    }
  }

  /// 请求 NAS 下载一部漫画（或指定章节）。
  Future<int?> requestDownload({
    required String sourceKey,
    required String comicId,
    String? title,
    List<String>? chapters,
  }) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.post('/api/v1/download', data: {
        'source': sourceKey,
        'comicId': comicId,
        if (title != null) 'title': title,
        if (chapters != null) 'chapters': chapters,
      });
      if (res.statusCode == 202 && res.data is Map) {
        return (res.data['jobId'] as num?)?.toInt();
      }
      return null;
    } catch (e) {
      return null;
    }
  }

  /// 删除 NAS 上某一章的缓存。
  Future<bool> deleteChapter({
    required String sourceKey,
    required String comicId,
    required String chapterId,
  }) async {
    if (!isEnabled) return false;
    final dio = _dio;
    if (dio == null) return false;
    try {
      var res = await dio.delete(
        '/api/v1/library/$sourceKey/${Uri.encodeComponent(comicId)}'
        '/${Uri.encodeComponent(chapterId)}',
      );
      return res.statusCode == 200;
    } catch (e) {
      return false;
    }
  }

  /// 删除 NAS 上一部漫画的全部缓存。
  Future<bool> deleteComic({
    required String sourceKey,
    required String comicId,
  }) async {
    if (!isEnabled) return false;
    final dio = _dio;
    if (dio == null) return false;
    try {
      var res = await dio.post('/api/v1/admin/comic/delete', data: {
        'source': sourceKey,
        'comicId': comicId,
      });
      return res.statusCode == 200;
    } catch (e) {
      return false;
    }
  }

  /// 列出 NAS 上的下载任务（供「下载管理」页使用）。
  Future<List<Map<String, dynamic>>> jobs({String? status, int limit = 200}) async {
    if (!isEnabled) return [];
    final dio = _dio;
    if (dio == null) return [];
    try {
      var res = await dio.get('/api/v1/admin/jobs', queryParameters: {
        if (status != null && status.isNotEmpty) 'status': status,
        'limit': limit,
      });
      if (res.statusCode != 200 || res.data is! Map) return [];
      var list = (res.data['jobs'] as List?) ?? [];
      return list.map((e) => Map<String, dynamic>.from(e as Map)).toList();
    } catch (e) {
      return [];
    }
  }

  /// 取消一个任务。
  Future<bool> cancelJob(int jobId) async {
    if (!isEnabled) return false;
    final dio = _dio;
    if (dio == null) return false;
    try {
      var res = await dio.post('/api/v1/jobs/$jobId/cancel');
      return res.statusCode == 200;
    } catch (e) {
      return false;
    }
  }

  /// 重试全部失败任务。
  Future<int> retryFailed({String? sourceKey}) async {
    if (!isEnabled) return 0;
    final dio = _dio;
    if (dio == null) return 0;
    try {
      var res = await dio.post('/api/v1/jobs/retry', data: {
        if (sourceKey != null) 'source': sourceKey,
      });
      if (res.statusCode == 200 && res.data is Map) {
        return (res.data['requeued'] as num?)?.toInt() ?? 0;
      }
      return 0;
    } catch (e) {
      return 0;
    }
  }

  /// 获取任务状态。
  Future<Map<String, dynamic>?> getJob(int jobId) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.get('/api/v1/jobs/$jobId');
      if (res.statusCode == 200 && res.data is Map) {
        return Map<String, dynamic>.from(res.data);
      }
      return null;
    } catch (e) {
      return null;
    }
  }

  /// 列出 NAS 库内容。
  Future<List<NasLibraryItem>> library() async {
    if (!isEnabled) return [];
    final dio = _dio;
    if (dio == null) return [];
    try {
      var res = await dio.get('/api/v1/library');
      if (res.statusCode != 200 || res.data is! Map) return [];
      var list = (res.data['library'] as List?) ?? [];
      return list.map((e) {
        var m = Map<String, dynamic>.from(e as Map);
        return NasLibraryItem(
          source: m['source']?.toString() ?? '',
          comicId: m['comicId']?.toString() ?? '',
          title: m['title']?.toString() ?? '',
          chapters: (m['chapters'] as num?)?.toInt() ?? 0,
          bytes: (m['bytes'] as num?)?.toInt() ?? 0,
        );
      }).toList();
    } catch (e) {
      return [];
    }
  }

  /// 拉取服务端所有漫画源的脚本。
  ///
  /// 用于新设备首次连接时自动补齐源：没有源，收藏列表和阅读都无法工作。
  Future<List<Map<String, dynamic>>> sourceBundles() async {
    if (!isEnabled) return [];
    final dio = _dio;
    if (dio == null) return [];
    try {
      var res = await dio.get(
        '/api/v1/sources/bundle',
        options: Options(receiveTimeout: const Duration(seconds: 60)),
      );
      if (res.statusCode != 200 || res.data is! Map) return [];
      var list = (res.data['sources'] as List?) ?? [];
      return list.map((e) => Map<String, dynamic>.from(e as Map)).toList();
    } catch (e) {
      return [];
    }
  }

  /// 服务端已持有的源清单（key → version）。
  ///
  /// 用于跳过未变更的源脚本上传：脚本单个几十 KB、几十个就是几百 KB，
  /// 每次启动都全量重传既慢又浪费。
  Future<Map<String, String>> sourceManifest() async {
    if (!isEnabled) return {};
    final dio = _dio;
    if (dio == null) return {};
    try {
      var res = await dio.get('/api/v1/sources');
      if (res.statusCode != 200 || res.data is! Map) return {};
      var list = (res.data['sources'] as List?) ?? [];
      final out = <String, String>{};
      for (final e in list) {
        final m = Map<String, dynamic>.from(e as Map);
        final key = m['key']?.toString() ?? '';
        if (key.isEmpty) continue;
        out[key] = m['version']?.toString() ?? '';
      }
      return out;
    } catch (e) {
      return {};
    }
  }

  /// 推送一批同步操作（与 NasSync.flush 走同一个端点，但允许调用方自带 clientId）。
  Future<bool> pushOps(List<Map<String, dynamic>> ops) async {
    if (!isEnabled || ops.isEmpty) return false;
    final dio = _dio;
    if (dio == null) return false;
    try {
      var res = await dio.post('/api/v1/sync/push', data: {
        'clientId': _config.clientId,
        'deviceName': _deviceName,
        'lamport': DateTime.now().millisecondsSinceEpoch,
        'ops': ops,
      });
      if (res.statusCode == 200 && res.data is Map) {
        final cursor = (res.data['cursor'] as num?)?.toInt();
        if (cursor != null) await updateCursor(cursor);
        return true;
      }
      return false;
    } catch (e) {
      return false;
    }
  }

  /// 设备名，仅用于服务端运维时区分来源。
  static String get _deviceName {
    try {
      if (Platform.isAndroid) return 'android';
      if (Platform.isIOS) return 'ios';
      if (Platform.isWindows) return 'windows';
      if (Platform.isMacOS) return 'macos';
      if (Platform.isLinux) return 'linux';
      return 'unknown';
    } catch (_) {
      return 'unknown';
    }
  }

  /// 从 NAS 取漫画详情（源站不可达时的兜底）。
  ///
  /// 返回的数据只包含 NAS 已知的部分：标题/封面/标签 + 已缓存的章节。
  /// 它不是源的完整详情（没有推荐、评论、点赞等），
  /// 但足以让详情页**不再是死循环 loading**。
  Future<NasComicFallback?> comicFallback({
    required String sourceKey,
    required String comicId,
  }) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.get('/api/v1/comic', queryParameters: {
        'source': sourceKey,
        'comicId': comicId,
      });
      if (res.statusCode != 200 || res.data is! Map) return null;
      final m = Map<String, dynamic>.from(res.data);
      if (m['cached'] != true) return null;
      return NasComicFallback.fromJson(m);
    } catch (e) {
      return null;
    }
  }

  /// 取 NAS 上这本漫画的封面字节（用于源站封面加载失败时的降级）。
  ///
  /// 走 /api/v1/comic 拿到封面 URL 再下载：封面扩展名由服务端落盘时的
  /// 魔数决定（png/jpg/webp 都可能），客户端猜不出来。
  Future<Uint8List?> fetchNasImage({
    required String sourceKey,
    required String comicId,
  }) async {
    if (!isEnabled) return null;
    final fb = await comicFallback(sourceKey: sourceKey, comicId: comicId);
    final coverUrl = fb?.cover ?? '';
    if (coverUrl.isEmpty) return null;
    // 只接受 NAS 自己的地址：源站封面可能同样不可达，
    // 在这里再试一次源站没有意义。
    if (!isNasUrl(coverUrl)) return null;

    final dio = _dio;
    if (dio == null) return null;
    try {
      final res = await dio.get<List<int>>(
        coverUrl,
        options: Options(responseType: ResponseType.bytes),
      );
      if (res.statusCode != 200) return null;
      final data = res.data;
      if (data == null || data.isEmpty) return null;
      return Uint8List.fromList(data);
    } catch (_) {
      return null;
    }
  }

  /// 取这本漫画在 NAS 上的缓存章节明细（管理面板用）。
  ///
  /// 与 cachedChapters（只返回 id 集合）的区别：这个带页数/大小/时间，
  /// 用于展示"占了多少空间、都是哪些章"。
  /// 返回 null 表示请求失败（离线/未配置），空列表表示确实没有缓存。
  Future<List<Map<String, dynamic>>?> comicChapters({
    required String sourceKey,
    required String comicId,
  }) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      final res = await dio.get('/api/v1/admin/chapters', queryParameters: {
        'source': sourceKey,
        'comicId': comicId,
      });
      if (res.statusCode != 200 || res.data is! Map) return null;
      final list = (res.data['chapters'] as List?) ?? [];
      return list
          .map((e) => Map<String, dynamic>.from(e as Map))
          .toList();
    } catch (_) {
      return null;
    }
  }

  /// 服务器信息（用于设置页展示）。
  Future<Map<String, dynamic>?> info() async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.get('/api/v1/info');
      if (res.statusCode == 200 && res.data is Map) {
        return Map<String, dynamic>.from(res.data);
      }
      return null;
    } catch (e) {
      return null;
    }
  }

  /// 更新本地记录的同步游标。
  Future<void> updateCursor(int cursor) async {
    if (_config.cursor == cursor) return;
    _config = _config.copyWith(cursor: cursor);
    appdata.settings['nas'] = _config.toJson();
    // 游标属于高频变更，不触发完整保存，仅写 implicitData 减少 IO
    appdata.implicitData['nasCursor'] = cursor;
  }

  /// 生成一个稳定的客户端 ID（首次使用时创建）。
  static String ensureClientId() {
    var id = appdata.settings['nasClientId'];
    if (id is String && id.isNotEmpty) return id;
    // 用 deviceId 派生，保证同一设备始终一致
    var deviceId = appdata.settings['deviceId']?.toString() ?? '';
    id = deviceId.isNotEmpty
        ? 'dev-${deviceId.substring(0, deviceId.length.clamp(0, 12))}'
        : 'dev-${DateTime.now().millisecondsSinceEpoch}';
    appdata.settings['nasClientId'] = id;
    return id;
  }
}

/// 收藏夹角标用的"NAS 已缓存漫画"索引。
///
/// 收藏夹列表可能有近千项，逐项查服务端显然不行；
/// 这里一次拉全库（source/comicId 对），内存里建 Set，UI 直接查。
/// 数据 60 秒过期——角标是辅助信息，不需要强一致。
class NasLibraryIndex {
  NasLibraryIndex._();

  static final NasLibraryIndex instance = NasLibraryIndex._();

  Set<String> _keys = {};
  DateTime _loadedAt = DateTime.fromMillisecondsSinceEpoch(0);
  bool _loading = false;

  static const _ttl = Duration(seconds: 60);

  bool _enabled() => NasClient.instance.isEnabled;

  /// 查询某本漫画是否已在 NAS 上缓存。
  bool contains(String sourceKey, String comicId) {
    if (!_enabled() || _keys.isEmpty) return false;
    return _keys.contains('$sourceKey\x00$comicId');
  }

  /// 确保索引已加载（过期自动重拉）。失败静默——角标不能阻塞列表。
  Future<void> ensure() async {
    if (!_enabled() || _loading) return;
    if (DateTime.now().difference(_loadedAt) < _ttl) return;
    _loading = true;
    try {
      final items = await NasClient.instance.library();
      _keys = items
          .where((e) => e.source.isNotEmpty && e.comicId.isNotEmpty)
          .map((e) => '${e.source}\x00${e.comicId}')
          .toSet();
      _loadedAt = DateTime.now();
    } catch (_) {
      // 静默：角标属增强信息
    } finally {
      _loading = false;
    }
  }

  /// 数据可能变化时（下载完成等）手动失效。
  void invalidate() {
    _loadedAt = DateTime.fromMillisecondsSinceEpoch(0);
  }
}
