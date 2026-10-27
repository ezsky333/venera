// 注意：这里刻意使用标准 Dio + 纯 Dart 的 IOHttpClientAdapter，
// 而不是项目里的 AppDio。
//
// 原因：AppDio 在构造时硬编码 `httpClientAdapter = RHttpAdapter()`，
// 依赖 flutter_rust_bridge（Rust 实现）。若 Rust 侧未初始化完成
// （例如冷启动早期、或某些 ABI 上初始化失败），所有请求都会抛
//   Bad state: flutter_rust_bridge has not been initialized
// NAS 是纯 HTTP/JSON，用系统 HTTP 栈更可靠，也避免和 Rust 生命周期耦合。
import 'dart:async';
import 'dart:convert' show jsonDecode, Utf8Decoder, LineSplitter;
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

/// 一页任务列表（与 /api/v1/admin/jobs 的返回对应）。
class NasJobPage {
  const NasJobPage({required this.jobs, required this.total});

  final List<Map<String, dynamic>> jobs;

  /// 服务端总数（不受本页 limit 限制），用于分页提示。
  final int total;

  bool get isEmpty => jobs.isEmpty;
}

/// 一次 NAS 下载请求的结果。
///
/// 服务端返回三种情形，客户端的反馈语必须不同：
///   - queued=true          → 排上了任务，可能只是补几章（见 [missing]）
///   - queue=false/up-to-date   → 全都是最新的，不需要下
///   - queued=false/already-queued → 已有同类任务在跑
class NasDownloadRequest {
  const NasDownloadRequest({
    required this.queued,
    this.jobId,
    this.total = 0,
    this.cached = 0,
    this.missing = 0,
    this.reason = '',
  });

  final bool queued;
  final int? jobId;

  /// 源端章节总数（0 = 章节表未知）。
  final int total;

  /// 服务端已缓存章节数。
  final int cached;

  /// 本次要补的章节数。
  final int missing;

  /// 未入队原因：up-to-date / already-queued。
  final String reason;

  bool get isUpToDate => !queued && reason == 'up-to-date';

  bool get isAlreadyQueued => !queued && reason == 'already-queued';
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
      // 游标以 implicitData 为准：它是每次同步后单独落盘的，
      // settings['nas'] 里的那份只在 saveData() 时才刷新，可能是旧的。
      final cursor = appdata.implicitData['nasCursor'];
      if (cursor is num && cursor.toInt() > _config.cursor) {
        _config = _config.copyWith(cursor: cursor.toInt());
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
  ///
  /// 服务端已改成**增量**语义：不传 chapters 时，它拿源端章节表与
  /// 已缓存集合求差集，只把缺失的章节写进任务。返回 [NasDownloadRequest]
  /// 而不是裸 jobId，因为"已经是最新"和"排上了任务"必须能被区分开——
  /// 旧实现只用 jobId != null 判断，服务端返回 409 already cached 时
  /// 客户端只会得到 null，用户看到"请求失败"，实际是本来就不用下。
  Future<NasDownloadRequest?> requestDownload({
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
      if (res.data is! Map) return null;
      final m = Map<String, dynamic>.from(res.data);
      if (res.statusCode == 202) {
        return NasDownloadRequest(
          queued: true,
          jobId: (m['jobId'] as num?)?.toInt(),
          total: (m['total'] as num?)?.toInt() ?? 0,
          cached: (m['cached'] as num?)?.toInt() ?? 0,
          missing: (m['missing'] as num?)?.toInt() ?? 0,
        );
      }
      if (res.statusCode == 200) {
        // 已是最新：不是失败
        return NasDownloadRequest(
          queued: false,
          reason: m['reason']?.toString() ?? 'up-to-date',
          total: (m['total'] as num?)?.toInt() ?? 0,
          cached: (m['cached'] as num?)?.toInt() ?? 0,
        );
      }
      if (res.statusCode == 409) {
        return NasDownloadRequest(
          queued: false,
          reason: m['reason']?.toString() ?? 'already-queued',
        );
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
  ///
  /// 走 /api/v1/admin/jobs：它同时返回 jobs 和 total，App 侧才能做分页
  /// 与"共 N 条"的提示（/api/v1/jobs 只有列表）。
  Future<NasJobPage> jobsPage({
    String? status,
    int limit = 50,
    int offset = 0,
  }) async {
    if (!isEnabled) return const NasJobPage(jobs: [], total: 0);
    final dio = _dio;
    if (dio == null) return const NasJobPage(jobs: [], total: 0);
    try {
      var res = await dio.get('/api/v1/admin/jobs', queryParameters: {
        if (status != null && status.isNotEmpty) 'status': status,
        'limit': limit,
        'offset': offset,
      });
      if (res.statusCode != 200 || res.data is! Map) {
        return const NasJobPage(jobs: [], total: 0);
      }
      final list = (res.data['jobs'] as List?) ?? [];
      final jobs = list.map((e) => Map<String, dynamic>.from(e as Map)).toList();
      final total = (res.data['total'] as num?)?.toInt() ?? jobs.length;
      return NasJobPage(jobs: jobs, total: total);
    } catch (e) {
      return const NasJobPage(jobs: [], total: 0);
    }
  }

  /// 列出 NAS 上的下载任务（只要列表的简版，兼容旧调用）。
  Future<List<Map<String, dynamic>>> jobs({String? status, int limit = 200}) async {
    final page = await jobsPage(status: status, limit: limit);
    return page.jobs;
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

  /// 对单个任务执行管理动作。
  ///
  /// 与 Web 管理面板共用同一套端点 `POST /api/v1/jobs/{id}/{action}`，
  /// 因此 App 里能做的和面板里完全一致：
  /// pause / resume / top / untop / cancel / retry / delete。
  ///
  /// 返回服务端的 `result` 文案；失败时返回 null（调用方给通用提示）。
  /// 注意 delete 对 running 任务会被服务端拒绝（409）：worker 靠 jobs
  /// 行里的 message 判断该不该停，行没了它就停不下来，必须先暂停。
  Future<String?> jobAction(int jobId, String action) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.post('/api/v1/jobs/$jobId/$action', data: {});
      if (res.statusCode == 200 && res.data is Map) {
        return res.data['result']?.toString() ?? action;
      }
      // 409 等冲突：把服务端的中文错误原样抛给 UI，比“操作失败”有用
      if (res.data is Map && res.data['error'] != null) {
        return '!${res.data['error']}';
      }
      return null;
    } catch (e) {
      return null;
    }
  }

  /// 批量操作：pause-all / resume-all / retry-failed / delete-finished。
  ///
  /// 返回受影响的任务数；失败返回 null（与 0 区分：0 是“确实没有可做的”）。
  Future<int?> bulkJobs(String action, {List<String>? statuses}) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.post('/api/v1/jobs/bulk', data: {
        'action': action,
        if (statuses != null) 'statuses': statuses,
      });
      if (res.statusCode != 200 || res.data is! Map) return null;
      return (res.data['affected'] as num?)?.toInt() ?? 0;
    } catch (e) {
      return null;
    }
  }

  /// 补录“收藏了但没缓存（或只缓存了一部分）”的漫画。
  ///
  /// dryRun=true 只拿计划，让用户先看到会入队多少本、跳过多少、
  /// 其中多少本是“只缺新章节”。收藏可能几百本，真没下过的也许
  /// 只有个位数，不给预览就只能盲点。
  Future<NasQueuePlan?> queueFavorites({bool dryRun = true}) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await (dryRun
          ? dio.get('/api/v1/jobs/queue-favorites',
              queryParameters: {'dryRun': '1'})
          : dio.post('/api/v1/jobs/queue-favorites', data: {}));
      if (res.statusCode != 200 || res.data is! Map) return null;
      return NasQueuePlan.fromJson(Map<String, dynamic>.from(res.data));
    } catch (e) {
      return null;
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

  /// 服务端总览（与 Web 面板顶部同一份数据）。
  Future<Map<String, dynamic>?> stats() async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      var res = await dio.get('/api/v1/admin/stats');
      if (res.statusCode == 200 && res.data is Map) {
        return Map<String, dynamic>.from(res.data);
      }
      return null;
    } catch (e) {
      return null;
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

  /// 订阅 NAS 任务的实时进度（SSE）。
  ///
  /// 服务端 `GET /api/v1/jobs/events` 推两类事件：
  ///   - `live`：实时进度帧（进度/页数/字节/速度），200ms 合并推送；
  ///   - `jobs`：任务列表快照（状态集合变化时）。
  ///
  /// 为什么不用轮询：进度每张图都在变，5 秒轮询既慢又浪费；
  /// 服务端已经把这些数据放在内存里按帧推送，端上直接用即可。
  ///
  /// 连接失败/断开时自动重连（指数退避，最长 30 秒）。
  /// 返回的 [NasJobEventStream] 必须 dispose，否则连接泄漏。
  NasJobEventStream subscribeJobs({String? status}) {
    return NasJobEventStream(
      baseUrl: _config.baseUrl,
      token: _config.token,
      status: status,
    );
  }

  /// 取某本漫画在 NAS 上的任务（含实时进度）。
  ///
  /// 与 [jobs] 的区别：那个列全部任务；这个只返回 (source, comicId)
  /// 匹配的任务，用于详情页的存储面板——用户要看的是"我这本下到哪了"。
  /// 返回 null 表示请求失败。
  Future<List<Map<String, dynamic>>?> comicJobs({
    required String sourceKey,
    required String comicId,
  }) async {
    if (!isEnabled) return null;
    final dio = _dio;
    if (dio == null) return null;
    try {
      final res = await dio.get('/api/v1/jobs/by-comic', queryParameters: {
        'source': sourceKey,
        'comicId': comicId,
      });
      if (res.statusCode != 200 || res.data is! Map) return null;
      final list = (res.data['jobs'] as List?) ?? [];
      return list.map((e) => Map<String, dynamic>.from(e as Map)).toList();
    } catch (_) {
      return null;
    }
  }

  /// 列出 NAS 库内容。
  Future<List<NasLibraryItem>> library() async {    if (!isEnabled) return [];
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
  ///
  /// 游标必须真正落盘，否则"同步过什么"这件事会在进程被杀后丢失：
  /// 下次启动从旧游标重新拉同一批操作，表现为每次开 App 都提示
  /// "检测到云端数据有更新"。实测就是 force-stop 后游标回到上一次的
  /// 值（服务端日志里同一批 op 被反复 pull）。
  ///
  /// 用 implicitData 而不是 settings 落盘：settings 走 appdata.saveData()
  /// 会把整个 appdata.json 重写一遍，而游标每次同步都会变。
  Future<void> updateCursor(int cursor) async {
    if (_config.cursor == cursor) return;
    _config = _config.copyWith(cursor: cursor);
    appdata.settings['nas'] = _config.toJson();
    appdata.implicitData['nasCursor'] = cursor;
    await appdata.writeImplicitData();
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

/// 补录计划的快照（对应服务端 jobs.QueuePlan）。
class NasQueuePlan {
  const NasQueuePlan({
    required this.candidates,
    required this.queued,
    required this.alreadyQueued,
    required this.alreadyCached,
    required this.incremental,
    required this.failed,
    required this.bySource,
    required this.items,
    required this.dryRun,
  });

  final int candidates;
  final int queued;
  final int alreadyQueued;
  final int alreadyCached;

  /// 其中"已缓存一部分、只缺新章节"的本数。
  ///
  /// 这个数不为 0 说明用户以前以为已经下全了，其实源站更新过。
  final int incremental;
  final int failed;
  final Map<String, int> bySource;
  final List<NasQueuePlanItem> items;
  final bool dryRun;

  static NasQueuePlan fromJson(Map<String, dynamic> m) {
    final bySource = <String, int>{};
    final raw = m['bySource'];
    if (raw is Map) {
      raw.forEach((k, v) {
        if (v is num) bySource[k.toString()] = v.toInt();
      });
    }
    final items = <NasQueuePlanItem>[];
    final rawItems = m['items'];
    if (rawItems is List) {
      for (final e in rawItems) {
        if (e is Map) {
          items.add(NasQueuePlanItem.fromJson(Map<String, dynamic>.from(e)));
        }
      }
    }
    int i(String k) => (m[k] as num?)?.toInt() ?? 0;
    return NasQueuePlan(
      candidates: i('candidates'),
      queued: i('queued'),
      alreadyQueued: i('alreadyQueued'),
      alreadyCached: i('alreadyCached'),
      incremental: i('incremental'),
      failed: i('failed'),
      bySource: bySource,
      items: items,
      dryRun: m['dryRun'] == true,
    );
  }
}

/// 补录计划里的一本漫画。
class NasQueuePlanItem {
  const NasQueuePlanItem({
    required this.source,
    required this.comicId,
    required this.title,
    required this.missing,
  });

  final String source;
  final String comicId;
  final String title;

  /// 本次要补的章节数；0 表示章节表未知（整本跑一遍）。
  final int missing;

  static NasQueuePlanItem fromJson(Map<String, dynamic> m) => NasQueuePlanItem(
        source: m['source']?.toString() ?? '',
        comicId: m['comicId']?.toString() ?? '',
        title: m['title']?.toString() ?? '',
        missing: (m['missing'] as num?)?.toInt() ?? 0,
      );
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

/// NAS 任务的实时进度快照（对应服务端 jobs.Live）。
class NasJobLive {
  const NasJobLive({
    required this.jobId,
    required this.progress,
    required this.chapterIndex,
    required this.chapterTotal,
    required this.chapterTitle,
    required this.imageDone,
    required this.imageTotal,
    required this.bytes,
    required this.speedBps,
    required this.status,
    required this.message,
    this.source = '',
    this.comicId = '',
  });

  final int jobId;
  final double progress;
  final int chapterIndex;
  final int chapterTotal;
  final String chapterTitle;
  final int imageDone;
  final int imageTotal;
  final int bytes;
  final double speedBps;
  final String status;
  final String message;

  /// 任务归属的漫画（详情页据此只显示自己这本的进度）。
  final String source;
  final String comicId;

  static NasJobLive fromJson(Map<String, dynamic> m) => NasJobLive(
        jobId: (m['jobId'] as num?)?.toInt() ?? 0,
        progress: (m['progress'] as num?)?.toDouble() ?? 0,
        chapterIndex: (m['chapterIndex'] as num?)?.toInt() ?? 0,
        chapterTotal: (m['chapterTotal'] as num?)?.toInt() ?? 0,
        chapterTitle: m['chapterTitle']?.toString() ?? '',
        imageDone: (m['imageDone'] as num?)?.toInt() ?? 0,
        imageTotal: (m['imageTotal'] as num?)?.toInt() ?? 0,
        bytes: (m['bytes'] as num?)?.toInt() ?? 0,
        speedBps: (m['speedBps'] as num?)?.toDouble() ?? 0,
        status: m['status']?.toString() ?? '',
        message: m['message']?.toString() ?? '',
        source: m['source']?.toString() ?? '',
        comicId: m['comicId']?.toString() ?? '',
      );
}

/// NAS 任务事件流（SSE）。
///
/// 用 dart:io 的 HttpClient 手工解析 `text/event-stream`：
/// Flutter 生态没有稳定的 SSE 包，而协议本身极简
/// （`event: x` / `data: y` 两行 + 空行分隔），手写反而更可控。
///
/// 断线自动重连（指数退避 1s→30s），dispose 后彻底停止。
class NasJobEventStream {
  NasJobEventStream({
    required this.baseUrl,
    required this.token,
    this.status,
  });

  final String baseUrl;
  final String token;
  final String? status;

  final _controller = StreamController<NasJobEvent>.broadcast();
  Stream<NasJobEvent> get stream => _controller.stream;

  HttpClient? _client;
  StreamSubscription<String>? _sub;
  bool _disposed = false;
  int _retry = 0;
  Timer? _retryTimer;

  /// 建立连接（可重复调用；内部会先断旧连接）。
  Future<void> connect() async {
    if (_disposed) return;
    await _closeConnection();
    if (_disposed) return;

    final uri = Uri.parse(
      '${baseUrl.replaceAll(RegExp(r'/+$'), '')}/api/v1/jobs/events'
      '?token=${Uri.encodeQueryComponent(token)}'
      '${status != null && status!.isNotEmpty ? '&status=${Uri.encodeQueryComponent(status!)}' : ''}',
    );

    try {
      final client = HttpClient()
        ..connectionTimeout = const Duration(seconds: 10)
        // 局域网直连，不能走用户代理
        ..findProxy = ((_) => 'DIRECT');
      _client = client;
      final req = await client.getUrl(uri);
      req.headers.set('Accept', 'text/event-stream');
      final resp = await req.close();
      if (resp.statusCode != 200) {
        throw StateError('SSE HTTP ${resp.statusCode}');
      }
      _retry = 0;

      // 按行解析 SSE；EventSource 的语义是事件之间用空行分隔
      final buffer = StringBuffer();
      String? evName;
      final dataLines = <String>[];

      _sub = resp
          .transform(const Utf8Decoder(allowMalformed: true))
          .transform(const LineSplitter())
          .listen(
        (line) {
          if (line.isEmpty) {
            // 一帧结束
            if (dataLines.isNotEmpty) {
              final raw = dataLines.join('\n');
              final name = evName ?? 'message';
              _emit(name, raw);
            }
            evName = null;
            dataLines.clear();
            return;
          }
          if (line.startsWith(':')) return; // 心跳注释
          if (line.startsWith('event:')) {
            evName = line.substring(6).trim();
          } else if (line.startsWith('data:')) {
            dataLines.add(line.substring(5).trimLeft());
          }
        },
        onError: (_) => _scheduleReconnect(),
        onDone: _scheduleReconnect,
        cancelOnError: true,
      );
      buffer.clear();
    } catch (_) {
      _scheduleReconnect();
    }
  }

  void _emit(String event, String raw) {
    if (_disposed) return;
    try {
      final decoded = jsonDecode(raw);
      if (event == 'live') {
        final list = (decoded as List)
            .whereType<Map>()
            .map((e) => NasJobLive.fromJson(Map<String, dynamic>.from(e)))
            .toList();
        _controller.add(NasJobEvent.live(list));
      } else if (event == 'jobs') {
        final list = (decoded as List)
            .whereType<Map>()
            .map((e) => Map<String, dynamic>.from(e))
            .toList();
        _controller.add(NasJobEvent.jobs(list));
      }
    } catch (_) {
      // 坏帧直接丢弃，不影响后续
    }
  }

  void _scheduleReconnect() {
    if (_disposed) return;
    _closeConnection();
    _retry = _retry < 5 ? _retry + 1 : 5;
    // 1s,2s,4s,8s,16s, 上限 30s
    final delay = Duration(seconds: (1 << (_retry - 1)).clamp(1, 30));
    _retryTimer?.cancel();
    _retryTimer = Timer(delay, () {
      if (!_disposed) connect();
    });
  }

  Future<void> _closeConnection() async {
    await _sub?.cancel();
    _sub = null;
    _client?.close(force: true);
    _client = null;
  }

  /// 停止订阅并释放连接。
  void dispose() {
    _disposed = true;
    _retryTimer?.cancel();
    _retryTimer = null;
    _closeConnection();
    _controller.close();
  }
}

/// 一帧任务事件：实时进度或任务列表。
class NasJobEvent {
  const NasJobEvent._(this.live, this.jobs);

  factory NasJobEvent.live(List<NasJobLive> items) =>
      NasJobEvent._(items, const []);
  factory NasJobEvent.jobs(List<Map<String, dynamic>> items) =>
      NasJobEvent._(const [], items);

  final List<NasJobLive> live;
  final List<Map<String, dynamic>> jobs;

  bool get isLive => live.isNotEmpty || jobs.isEmpty;
}
