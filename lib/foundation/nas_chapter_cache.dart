import 'dart:async';

import 'package:flutter/foundation.dart';

import 'package:venera/foundation/nas_client.dart';

/// 详情页用的 NAS 章节缓存状态。
///
/// 职责：
///   1. 打开详情页时拉一次"这本书有哪些章已在 NAS"；
///   2. 结果短期缓存，避免反复进出详情页重复请求；
///   3. 触发 NAS 下载后把新章节增量并入，让角标及时更新；
///   4. 暴露整本/单章的下载与删除操作给 UI。
///
/// 设计原则：NAS 不可用时**一律降级为"没有缓存"**，绝不阻塞详情页渲染。
class NasChapterCache extends ChangeNotifier {
  NasChapterCache({
    required this.sourceKey,
    required this.comicId,
    this.title,
  });

  final String sourceKey;
  final String comicId;
  final String? title;

  /// 已缓存到 NAS 的章节 ID 集合。
  Set<String> _cached = {};
  Set<String> get cachedChapters => _cached;

  bool _loading = false;
  bool get isLoading => _loading;

  /// NAS 是否在线可用。离线时 UI 不显示任何 NAS 相关标识。
  bool _available = false;
  bool get isAvailable => _available;

  /// 正在为哪些章节请求下载（UI 显示转圈）。
  final Set<String> _requesting = {};
  bool isRequesting(String chapterId) => _requesting.contains(chapterId);

  /// 整本下载进行中。
  bool _bulkRequesting = false;
  bool get isBulkRequesting => _bulkRequesting;

  bool _disposed = false;

  /// 同一本书的缓存，跨页面实例复用（30 秒 TTL）。
  static final Map<String, _CacheEntry> _memo = {};
  static const _memoTtl = Duration(seconds: 30);

  String get _key => '$sourceKey/$comicId';

  /// 载入缓存状态（带 memo）。
  Future<void> load({bool force = false}) async {
    if (!NasClient.instance.isEnabled) {
      _available = false;
      _safeNotify();
      return;
    }

    if (!force) {
      final hit = _memo[_key];
      if (hit != null && hit.isFresh) {
        _cached = Set.of(hit.chapters);
        _available = hit.available;
        _safeNotify();
        return;
      }
    }

    _loading = true;
    _safeNotify();

    final ok = await NasClient.instance.probe();
    if (!ok) {
      _available = false;
      _cached = {};
      _loading = false;
      _safeNotify();
      return;
    }

    _available = true;
    final chapters = await NasClient.instance.cachedChapters(sourceKey, comicId);
    _cached = chapters;
    _memo[_key] = _CacheEntry(chapters, true);
    _loading = false;
    _safeNotify();
  }

  /// 请求 NAS 下载指定章节（为空表示整本）。
  Future<bool> requestDownload({List<String>? chapters}) async {
    if (!NasClient.instance.isEnabled) return false;

    final targets = chapters ?? const <String>[];
    if (targets.isEmpty) {
      if (_bulkRequesting) return false;
      _bulkRequesting = true;
    } else {
      _requesting.addAll(targets);
    }
    _safeNotify();

    int? jobId;
    try {
      jobId = await NasClient.instance.requestDownload(
        sourceKey: sourceKey,
        comicId: comicId,
        title: title,
        chapters: targets.isEmpty ? null : targets,
      );
    } finally {
      _bulkRequesting = false;
      _requesting.removeAll(targets);
    }

    if (jobId != null) {
      // 下载会异步完成；先按"已请求"乐观标记，稍后再刷新真实状态
      if (targets.isEmpty) {
        // 整本：不猜具体章节，等下一轮刷新
        unawaited(_refreshLater());
      } else {
        _cached = {..._cached, ...targets};
        _memo[_key] = _CacheEntry(_cached, true);
      }
      _safeNotify();
    }
    return jobId != null;
  }

  /// 删除 NAS 上某章的缓存。
  Future<bool> deleteChapter(String chapterId) async {
    final ok = await NasClient.instance.deleteChapter(
      sourceKey: sourceKey,
      comicId: comicId,
      chapterId: chapterId,
    );
    if (ok) {
      _cached = Set.of(_cached)..remove(chapterId);
      _memo[_key] = _CacheEntry(_cached, true);
      _safeNotify();
    }
    return ok;
  }

  /// 删除整本缓存。
  Future<bool> deleteComic() async {
    final ok = await NasClient.instance.deleteComic(
      sourceKey: sourceKey,
      comicId: comicId,
    );
    if (ok) {
      _cached = {};
      _memo.remove(_key);
      _safeNotify();
    }
    return ok;
  }

  /// 轮询任务完成情况，完成后刷新一次真实状态。
  ///
  /// 下载是服务端后台任务，客户端不持有进度流，因此用有限次轮询
  /// 代替（最多 60 次 × 3 秒）。轮询失败不影响 UI。
  Future<void> _refreshLater() async {
    for (var i = 0; i < 60; i++) {
      await Future.delayed(const Duration(seconds: 3));
      if (_disposed) return;
      final chapters =
          await NasClient.instance.cachedChapters(sourceKey, comicId);
      if (chapters.length != _cached.length) {
        _cached = chapters;
        _memo[_key] = _CacheEntry(chapters, true);
        _safeNotify();
      }
    }
  }

  /// 外部手动刷新（下拉刷新 / 从设置页返回）。
  Future<void> refresh() => load(force: true);

  /// 清除某本书的 memo（下载完成后由别处调用）。
  static void invalidate(String sourceKey, String comicId) {
    _memo.remove('$sourceKey/$comicId');
  }

  void _safeNotify() {
    if (!_disposed) notifyListeners();
  }

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }
}

class _CacheEntry {
  _CacheEntry(this.chapters, this.available) : at = DateTime.now();
  final Set<String> chapters;
  final bool available;
  final DateTime at;

  bool get isFresh => DateTime.now().difference(at) < NasChapterCache._memoTtl;
}
