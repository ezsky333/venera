import 'dart:async';

import 'package:venera/foundation/appdata.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/network/app_dio.dart';

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

  void _rebuildDio() {
    if (!_config.isValid) {
      _dio = null;
      return;
    }
    _dio = AppDio(BaseOptions(
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
