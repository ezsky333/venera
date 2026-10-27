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

  /// 判断某章是否已缓存，兼容"整本作为一个章节"的服务端记录。
  ///
  /// 背景：画廊型源（wnacg 等）没有章节列表，服务端下载时把整本
  /// 存成 chapter_id == comicId 的单条记录。而客户端在有章节列表的
  /// 情况下按真实章节 id 比对，两边 key 体系不同 → 永远匹配不上，
  /// 表现为"已缓存却显示未缓存、还能反复触发下载"。
  ///
  /// 因此：真实章节 id 命中，或集合里存在"整本记录"（key == comicId），
  /// 都算已缓存。
  bool isCached(String chapterId) {
    if (_cached.contains(chapterId)) return true;
    // 整本记录：服务端用 comicId 作为唯一章节 id
    if (_cached.contains(comicId)) return true;
    return false;
  }

  /// 是否缓存了"整本"（服务端合成的单章记录）。
  bool get hasWholeBookRecord => _cached.contains(comicId);

  /// 仅供测试：直接替换内部缓存集合，避免依赖网络。
  @visibleForTesting
  void replaceCached(Set<String> chapters) {
    _cached = Set.of(chapters);
    _memo[_key] = _CacheEntry(_cached, true);
    _safeNotify();
  }

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

  /// 请求 NAS 下载指定章节（为空表示"补齐所有缺失章节"）。
  ///
  /// 返回服务端的判定结果，而不是 bool。原因：服务端现在做增量，
  /// 不传 chapters 时可能返回
  ///   - queued=true, missing=2  → 只补了 2 章（这本以前下过）
  ///   - queued=false/up-to-date → 已经是最新，什么都没做
  ///   - queued=false/already-queued → 已有同类任务在跑
  /// 三种情况给用户的话术必须不同，否则"点了一下却没反应"会被当成 bug。
  Future<NasDownloadRequest?> requestDownload({List<String>? chapters}) async {
    if (!NasClient.instance.isEnabled) return null;

    final targets = chapters ?? const <String>[];
    if (targets.isEmpty) {
      if (_bulkRequesting) return null;
      _bulkRequesting = true;
    } else {
      _requesting.addAll(targets);
    }
    _safeNotify();

    NasDownloadRequest? result;
    try {
      result = await NasClient.instance.requestDownload(
        sourceKey: sourceKey,
        comicId: comicId,
        title: title,
        chapters: targets.isEmpty ? null : targets,
      );
    } finally {
      _bulkRequesting = false;
      _requesting.removeAll(targets);
    }

    if (result != null && result.queued) {
      // 下载会异步完成；先按"已请求"乐观标记，稍后再刷新真实状态
      if (targets.isEmpty) {
        // 增量：不猜具体章节，等下一轮刷新
        unawaited(_refreshLater());
      } else {
        _cached = {..._cached, ...targets};
        _memo[_key] = _CacheEntry(_cached, true);
      }
      _safeNotify();
    }
    return result;
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
  /// 下载是服务端后台任务，客户端不持有进度流，因此用轮询代替。
  ///
  /// 时长取舍：原先是 60 次 × 3 秒（共 3 分钟），但真实漫画常有
  /// 150+ 页、200MB+（实测《契薬交唇》153 页 233MB），3 分钟根本
  /// 下不完，轮询结束后 UI 永远停在"未缓存"。这里放宽到 20 分钟
  /// 并把首轮间隔缩短（前 10 次 1 秒），小文件能很快看到结果。
  ///
  /// 另外：无论成功与否，结束时要清掉 memo，避免把"过期空结果"
  /// 缓存 30 秒继续误导用户。
  Future<void> _refreshLater() async {
    var sawChange = false;
    for (var i = 0; i < 240; i++) {
      final wait = i < 10 ? const Duration(seconds: 1) : const Duration(seconds: 5);
      await Future.delayed(wait);
      if (_disposed) return;
      final chapters =
          await NasClient.instance.cachedChapters(sourceKey, comicId);
      if (chapters.length != _cached.length || !_sameSet(chapters, _cached)) {
        _cached = chapters;
        _memo[_key] = _CacheEntry(chapters, true);
        sawChange = true;
        _safeNotify();
        // 服务端已落库（整本记录或所有章节都在），可以停了
        if (chapters.isNotEmpty && !_loading) break;
      }
    }
    if (!sawChange) {
      // 没有任何变化：清 memo，让下次进入强制重查（而不是吃 30 秒旧值）
      _memo.remove(_key);
      _safeNotify();
    }
  }

  static bool _sameSet(Set<String> a, Set<String> b) {
    if (a.length != b.length) return false;
    for (final e in a) {
      if (!b.contains(e)) return false;
    }
    return true;
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
