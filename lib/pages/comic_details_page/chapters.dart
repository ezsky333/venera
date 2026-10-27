part of 'comic_page.dart';

class _ComicChapters extends StatelessWidget {
  const _ComicChapters({
    this.history,
    required this.groupedMode,
    this.nasCache,
    this.nasUncached,
  });

  final History? history;

  final bool groupedMode;

  /// NAS 章节缓存状态；为空表示未启用 NAS 或尚未就绪。
  final NasChapterCache? nasCache;

  /// 明确**未缓存**的章节 id（仅在 NAS 兜底渲染时非空）。
  ///
  /// 兜底详情页返回的是源端全量章节表，其中大部分并没有下载到 NAS。
  /// 这些章节要灰掉：点了也是"这一章没有缓存在 NAS 上"，不如提前说明。
  final Set<String>? nasUncached;

  @override
  Widget build(BuildContext context) {
    return groupedMode
        ? _GroupedComicChapters(history, nasCache, nasUncached)
        : _NormalComicChapters(history, nasCache, nasUncached);
  }
}

/// 章节卡片右上角的 NAS 缓存角标。
///
/// 三种状态的视觉区分：
///   - 已缓存：实心云图标 + 主色调，用户一眼能看出"这本我NAS上有"；
///   - 请求中：转圈，给出即时反馈；
///   - 未缓存：不显示，保持界面干净（不制造"我该点它"的暗示）。
class _NasBadge extends StatelessWidget {
  const _NasBadge({
    required this.cached,
    required this.requesting,
  });

  final bool cached;
  final bool requesting;

  @override
  Widget build(BuildContext context) {
    if (!cached && !requesting) return const SizedBox.shrink();
    final color = cached ? context.colorScheme.primary : context.colorScheme.outline;
    return Padding(
      padding: const EdgeInsets.only(left: 4),
      child: requesting
          ? SizedBox(
              width: 12,
              height: 12,
              child: CircularProgressIndicator(strokeWidth: 1.6, color: color),
            )
          : Icon(Icons.cloud_done, size: 14, color: color),
    );
  }
}

class _NormalComicChapters extends StatefulWidget {
  const _NormalComicChapters(this.history, this.nasCache, this.nasUncached);

  final History? history;

  final NasChapterCache? nasCache;

  final Set<String>? nasUncached;

  @override
  State<_NormalComicChapters> createState() => _NormalComicChaptersState();
}

class _NormalComicChaptersState extends State<_NormalComicChapters> {
  late _ComicPageState state;

  late bool reverse;

  bool showAll = false;

  late History? history;

  late ComicChapters chapters;

  @override
  void initState() {
    super.initState();
    reverse = appdata.settings["reverseChapterOrder"] ?? false;
    history = widget.history;
  }

  @override
  void didChangeDependencies() {
    state = context.findAncestorStateOfType<_ComicPageState>()!;
    chapters = state.comic.chapters!;
    super.didChangeDependencies();
  }

  @override
  void didUpdateWidget(covariant _NormalComicChapters oldWidget) {
    super.didUpdateWidget(oldWidget);
    setState(() {
      history = widget.history;
    });
  }

  @override
  Widget build(BuildContext context) {
    return SliverLayoutBuilder(
      builder: (context, constrains) {
        int length = chapters.length;
        bool canShowAll = showAll;
        if (!showAll) {
          var width = constrains.crossAxisExtent - 16;
          var crossItems = width ~/ 200;
          if (width % 200 != 0) {
            crossItems += 1;
          }
          length = math.min(length, crossItems * 8);
          if (length == chapters.length) {
            canShowAll = true;
          }
        }

        return SliverMainAxisGroup(
          slivers: [
            SliverToBoxAdapter(
              child: ListTile(
                title: Text("Chapters".tl),
                trailing: Tooltip(
                  message: "Order".tl,
                  child: IconButton(
                    icon: Icon(reverse
                        ? Icons.vertical_align_top
                        : Icons.vertical_align_bottom_outlined),
                    onPressed: () {
                      setState(() {
                        reverse = !reverse;
                      });
                    },
                  ),
                ),
              ),
            ),
            SliverGrid(
              delegate: SliverChildBuilderDelegate(
                childCount: length,
                (context, i) {
                  if (reverse) {
                    i = chapters.length - i - 1;
                  }
                  var key = chapters.ids.elementAt(i);
                  var value = chapters[key]!;
                  bool visited = (history?.readEpisode ?? {}).contains(i + 1);
                  final nas = widget.nasCache;
                  final cached = nas?.isCached(key) ?? false;
                  final requesting = nas?.isRequesting(key) ?? false;
                  // 兜底渲染时，明确未缓存的章节要灰掉
                  final uncached =
                      widget.nasUncached?.contains(key) ?? false;
                  return Padding(
                    padding: const EdgeInsets.fromLTRB(4, 4, 4, 4),
                    child: Material(
                      color: cached
                          ? context.colorScheme.primaryContainer.withValues(alpha: 0.5)
                          : context.colorScheme.surfaceContainer,
                      borderRadius: BorderRadius.circular(16),
                      child: InkWell(
                        onTap: () => state.read(i + 1),
                        onLongPress: nas == null || !nas.isAvailable
                            ? null
                            : () => _showNasChapterMenu(context, nas, key, cached),
                        borderRadius: BorderRadius.circular(16),
                        child: Padding(
                          padding: const EdgeInsets.symmetric(horizontal: 8),
                          child: Row(
                            mainAxisAlignment: MainAxisAlignment.center,
                            children: [
                              Flexible(
                                child: Text(
                                  value,
                                  maxLines: 1,
                                  textAlign: TextAlign.center,
                                  overflow: TextOverflow.ellipsis,
                                  style: TextStyle(
                                    color: uncached
                                        ? context.colorScheme.outline
                                            .withValues(alpha: 0.6)
                                        : (visited
                                            ? context.colorScheme.outline
                                            : null),
                                  ),
                                ),
                              ),
                              _NasBadge(cached: cached, requesting: requesting),
                            ],
                          ),
                        ),
                      ),
                    ),
                  );
                },
              ),
              gridDelegate: const SliverGridDelegateWithFixedHeight(
                maxCrossAxisExtent: 250,
                itemHeight: 48,
              ),
            ).sliverPadding(const EdgeInsets.symmetric(horizontal: 8)),
            if (!canShowAll)
              SliverToBoxAdapter(
                child: Align(
                  alignment: Alignment.center,
                  child: TextButton.icon(
                    icon: const Icon(Icons.arrow_drop_down),
                    onPressed: () {
                      setState(() {
                        showAll = true;
                      });
                    },
                    label: Text("${"Show all".tl} (${chapters.length})"),
                  ).paddingTop(12),
                ),
              ),
            const SliverToBoxAdapter(
              child: Divider(),
            ),
          ],
        );
      },
    );
  }
}

class _GroupedComicChapters extends StatefulWidget {
  const _GroupedComicChapters(
      this.history, this.nasCache, this.nasUncached);

  final History? history;

  final NasChapterCache? nasCache;

  final Set<String>? nasUncached;

  @override
  State<_GroupedComicChapters> createState() => _GroupedComicChaptersState();
}

class _GroupedComicChaptersState extends State<_GroupedComicChapters>
    with SingleTickerProviderStateMixin {
  late _ComicPageState state;

  late bool reverse;

  bool showAll = false;

  late History? history;

  late ComicChapters chapters;

  late TabController tabController;

  late int index;

  @override
  void initState() {
    super.initState();
    reverse = appdata.settings["reverseChapterOrder"] ?? false;
    history = widget.history;
    if (history?.group != null) {
      index = history!.group! - 1;
    } else {
      index = 0;
    }
  }

  @override
  void didChangeDependencies() {
    state = context.findAncestorStateOfType<_ComicPageState>()!;
    chapters = state.comic.chapters!;
    tabController = TabController(
      initialIndex: index,
      length: chapters.ids.length,
      vsync: this,
    );
    tabController.addListener(onTabChange);
    super.didChangeDependencies();
  }

  void onTabChange() {
    if (index != tabController.index) {
      setState(() {
        index = tabController.index;
      });
    }
  }

  @override
  void didUpdateWidget(covariant _GroupedComicChapters oldWidget) {
    super.didUpdateWidget(oldWidget);
    setState(() {
      history = widget.history;
    });
  }

  @override
  Widget build(BuildContext context) {
    return SliverLayoutBuilder(
      builder: (context, constrains) {
        var group = chapters.getGroupByIndex(index);
        int length = group.length;
        bool canShowAll = showAll;
        if (!showAll) {
          var width = constrains.crossAxisExtent - 16;
          var crossItems = width ~/ 200;
          if (width % 200 != 0) {
            crossItems += 1;
          }
          length = math.min(length, crossItems * 8);
          if (length == group.length) {
            canShowAll = true;
          }
        }

        return SliverMainAxisGroup(
          slivers: [
            SliverToBoxAdapter(
              child: ListTile(
                title: Text("Chapters".tl),
                trailing: Tooltip(
                  message: "Order".tl,
                  child: IconButton(
                    icon: Icon(reverse
                        ? Icons.vertical_align_top
                        : Icons.vertical_align_bottom_outlined),
                    onPressed: () {
                      setState(() {
                        reverse = !reverse;
                      });
                    },
                  ),
                ),
              ),
            ),
            SliverToBoxAdapter(
              child: AppTabBar(
                withUnderLine: false,
                controller: tabController,
                tabs: chapters.groups.map((e) => Tab(text: e)).toList(),
              ),
            ),
            SliverPadding(padding: const EdgeInsets.only(top: 8)),
            SliverGrid(
              delegate: SliverChildBuilderDelegate(
                childCount: length,
                (context, i) {
                  if (reverse) {
                    i = group.length - i - 1;
                  }
                  var key = group.keys.elementAt(i);
                  var value = group[key]!;
                  var chapterIndex = 0;
                  for (var j = 0; j < chapters.groupCount; j++) {
                    if (j == index) {
                      chapterIndex += i;
                      break;
                    }
                    chapterIndex += chapters.getGroupByIndex(j).length;
                  }
                  String rawIndex = (chapterIndex + 1).toString();
                  String groupedIndex = "${index + 1}-${i + 1}";
                  bool visited = false;
                  if (history != null) {
                    visited = history!.readEpisode.contains(groupedIndex) ||
                        history!.readEpisode.contains(rawIndex);
                  }
                  final nas = widget.nasCache;
                  final cached = nas?.isCached(key) ?? false;
                  final requesting = nas?.isRequesting(key) ?? false;
                  final uncached =
                      widget.nasUncached?.contains(key) ?? false;
                  return Padding(
                    padding: const EdgeInsets.fromLTRB(4, 4, 4, 4),
                    child: Material(
                      color: cached
                          ? context.colorScheme.primaryContainer.withValues(alpha: 0.5)
                          : context.colorScheme.surfaceContainerLow,
                      borderRadius: BorderRadius.circular(12),
                      child: InkWell(
                        onTap: () => state.read(chapterIndex + 1),
                        onLongPress: nas == null || !nas.isAvailable
                            ? null
                            : () => _showNasChapterMenu(context, nas, key, cached),
                        borderRadius: BorderRadius.circular(12),
                        child: Padding(
                          padding: const EdgeInsets.symmetric(horizontal: 8),
                          child: Row(
                            mainAxisAlignment: MainAxisAlignment.center,
                            children: [
                              Flexible(
                                child: Text(
                                  value,
                                  maxLines: 1,
                                  textAlign: TextAlign.center,
                                  overflow: TextOverflow.ellipsis,
                                  style: TextStyle(
                                    color: uncached
                                        ? context.colorScheme.outline
                                            .withValues(alpha: 0.6)
                                        : (visited
                                            ? context.colorScheme.outline
                                            : null),
                                  ),
                                ),
                              ),
                              _NasBadge(cached: cached, requesting: requesting),
                            ],
                          ),
                        ),
                      ),
                    ),
                  );
                },
              ),
              gridDelegate: const SliverGridDelegateWithFixedHeight(
                maxCrossAxisExtent: 250,
                itemHeight: 48,
              ),
            ).sliverPadding(const EdgeInsets.symmetric(horizontal: 8)),
            if (!canShowAll)
              SliverToBoxAdapter(
                child: Align(
                  alignment: Alignment.center,
                  child: TextButton.icon(
                    icon: const Icon(Icons.arrow_drop_down),
                    onPressed: () {
                      setState(() {
                        showAll = true;
                      });
                    },
                    label: Text("${"Show all".tl} (${group.length})"),
                  ).paddingTop(12),
                ),
              ),
            const SliverToBoxAdapter(
              child: Divider(),
            ),
          ],
        );
      },
    );
  }
}

/// 长按章节弹出的 NAS 操作菜单。
///
/// 为什么用长按而不是给每个章节加按钮：
/// 章节网格本身已经很密（一屏几十个），额外按钮会显著降低可扫读性。
/// 长按是移动端的约定俗成，且完全不打扰正常阅读路径。
void _showNasChapterMenu(
  BuildContext context,
  NasChapterCache cache,
  String chapterId,
  bool cached,
) {
  showModalBottomSheet(
    context: context,
    builder: (ctx) => SafeArea(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          ListTile(
            leading: const Icon(Icons.cloud_download_outlined),
            title: Text("Download to NAS".tl),
            subtitle: cached ? Text("Already cached".tl) : null,
            enabled: !cached,
            onTap: () async {
              Navigator.of(ctx).pop();
              final res = await cache.requestDownload(chapters: [chapterId]);
              if (!context.mounted) return;
              if (res == null) {
                context.showMessage(
                  message: "Failed to request NAS download".tl,
                );
              } else if (res.queued) {
                context.showMessage(message: "Queued download to NAS".tl);
              } else if (res.isAlreadyQueued) {
                context.showMessage(message: "Queued on NAS".tl);
              } else {
                context.showMessage(message: "Already cached on NAS".tl);
              }
            },
          ),
          if (cached)
            ListTile(
              leading: const Icon(Icons.delete_outline),
              title: Text("Delete from NAS".tl),
              onTap: () async {
                Navigator.of(ctx).pop();
                final ok = await cache.deleteChapter(chapterId);
                if (!context.mounted) return;
                context.showMessage(
                  message: ok
                      ? "Deleted from NAS".tl
                      : "Failed to delete from NAS".tl,
                );
              },
            ),
        ],
      ),
    ),
  );
}


/// 单章漫画的 NAS 缓存信息条。
///
/// 为什么需要它：绅士漫画这类源如果只有一章，会**不返回章节列表**、
/// 只给预览图。此时 `_ComicChapters` 整个不渲染，
/// "这一章在 NAS 上有没有缓存"就没有任何地方可以展示
/// （章节卡片上的云图标不存在了）。
///
/// 这里用一条横向信息栏表达同样的信息，并直接给出操作：
///   - 未缓存：显示「未缓存」+ 下载按钮
///   - 已缓存：显示「已缓存 N 页」+ 阅读/删除按钮
class _NasCacheInfoBar extends StatefulWidget {
  const _NasCacheInfoBar({
    required this.nasCache,
    required this.comicId,
    required this.sourceKey,
    required this.title,
  });

  final NasChapterCache? nasCache;
  final String comicId;
  final String sourceKey;
  final String title;

  @override
  State<_NasCacheInfoBar> createState() => _NasCacheInfoBarState();
}

class _NasCacheInfoBarState extends State<_NasCacheInfoBar> {
  NasChapterCache? _cache;
  bool _busy = false;

  @override
  void initState() {
    super.initState();
    _cache = widget.nasCache;
    _cache?.addListener(_onChanged);
    // 单章漫画的"章节 id"就是漫画 id（源的 loadEp(comicId, null) 约定）
    _cache?.load();
  }

  @override
  void dispose() {
    _cache?.removeListener(_onChanged);
    super.dispose();
  }

  void _onChanged() {
    if (mounted) setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    final cache = _cache;
    if (cache == null || !cache.isAvailable) {
      return const SliverPadding(padding: EdgeInsets.zero);
    }

    // 单章场景：用漫画 id 作为章节 key 查询
    final chapterId = widget.comicId;
    final cached = cache.isCached(chapterId);
    final requesting = cache.isRequesting(chapterId);

    return SliverToBoxAdapter(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 4, 16, 4),
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
          decoration: BoxDecoration(
            color: cached
                ? context.colorScheme.primaryContainer.withValues(alpha: 0.4)
                : context.colorScheme.surfaceContainerHigh,
            borderRadius: BorderRadius.circular(12),
          ),
          child: Row(
            children: [
              Icon(
                cached ? Icons.cloud_done : Icons.cloud_queue,
                size: 18,
                color: cached
                    ? context.colorScheme.primary
                    : context.colorScheme.outline,
              ),
              const SizedBox(width: 10),
              Expanded(
                child: Text(
                  cached ? "Cached on NAS".tl : "Not cached on NAS".tl,
                  style: const TextStyle(fontSize: 13),
                ),
              ),
              if (requesting)
                const SizedBox(
                  width: 16,
                  height: 16,
                  child: CircularProgressIndicator(strokeWidth: 2),
                )
              else if (!cached)
                TextButton(
                  onPressed: _busy ? null : () => _download(chapterId),
                  child: Text("Download to NAS".tl),
                )
              else ...[
                TextButton(
                  onPressed: _busy ? null : () => _download(chapterId, force: true),
                  child: Text("Re-download".tl),
                ),
                TextButton(
                  onPressed: _busy ? null : () => _delete(chapterId),
                  child: Text("Delete from NAS".tl),
                ),
              ],
            ],
          ),
        ),
      ),
    );
  }

  Future<void> _download(String chapterId, {bool force = false}) async {
    final cache = _cache;
    if (cache == null) return;
    setState(() => _busy = true);
    final res = await cache.requestDownload(chapters: [chapterId]);
    if (!mounted) return;
    setState(() => _busy = false);
    if (res == null) {
      context.showMessage(message: "Failed to request NAS download".tl);
    } else if (res.queued) {
      context.showMessage(message: "Queued download to NAS".tl);
    } else if (res.isAlreadyQueued) {
      context.showMessage(message: "Queued on NAS".tl);
    } else {
      context.showMessage(message: "Already cached on NAS".tl);
    }
  }

  Future<void> _delete(String chapterId) async {
    final cache = _cache;
    if (cache == null) return;
    setState(() => _busy = true);
    final ok = await cache.deleteChapter(chapterId);
    if (!mounted) return;
    setState(() => _busy = false);
    context.showMessage(
      message: ok ? "Deleted from NAS".tl : "Failed to delete from NAS".tl,
    );
  }
}


/// NAS 存储管理面板：列出这本漫画在 NAS 上缓存的全部章节。
///
/// 交互设计：
///   - 顶部汇总（章节数 / 总大小），让用户一眼知道占了多少空间；
///   - 每章一行，带页数和删除按钮；
///   - 底部提供"删除全部"，对应"这本书我不看了，清掉"。
///
/// 删除是不可逆的（服务端会删磁盘文件），因此"删除全部"要求二次确认，
/// 单章删除不确认（误删可以重新下载，且操作成本低）。
class _NasStoragePanel extends StatefulWidget {
  const _NasStoragePanel({
    required this.cache,
    required this.comicTitle,
    required this.comicId,
    required this.sourceKey,
  });

  final NasChapterCache cache;
  final String comicTitle;
  final String comicId;
  final String sourceKey;

  @override
  State<_NasStoragePanel> createState() => _NasStoragePanelState();
}

class _NasStoragePanelState extends State<_NasStoragePanel> {
  /// 章节详情（id → 页数/大小），从服务端拉一次
  List<Map<String, dynamic>>? _chapters;
  bool _loading = true;
  String? _error;
  final Set<String> _deleting = {};

  /// 这本漫画的任务（下载中/排队/失败），带实时进度。
  List<Map<String, dynamic>> _jobs = [];

  /// SSE 订阅：只关心本漫画的 live 帧，用来刷新任务进度。
  NasJobEventStream? _events;
  StreamSubscription<NasJobEvent>? _eventSub;

  /// jobId → 实时状态
  Map<int, NasJobLive> _live = {};

  @override
  void initState() {
    super.initState();
    _loadAll();
    _connectEvents();
  }

  @override
  void dispose() {
    _eventSub?.cancel();
    _events?.dispose();
    super.dispose();
  }

  /// 订阅任务事件流：进度实时刷新，状态变化时重拉任务列表。
  void _connectEvents() {
    if (!NasClient.instance.isEnabled) return;
    _events?.dispose();
    _events = NasClient.instance.subscribeJobs();
    _eventSub?.cancel();
    _eventSub = _events!.stream.listen((ev) {
      if (!mounted) return;
      if (ev.live.isNotEmpty) {
        // 只保留本漫画的任务帧
        final mine = <int, NasJobLive>{};
        for (final l in ev.live) {
          if (l.source == widget.sourceKey && l.comicId == widget.comicId) {
            mine[l.jobId] = l;
          }
        }
        if (mine.isEmpty && _live.isEmpty) return;
        setState(() => _live = mine);
      } else {
        // 任务集合可能变化（新入队/完成），重拉一次
        _loadJobs(silent: true);
      }
    });
    _events!.connect();
  }

  Future<void> _loadAll() async {
    await Future.wait([_loadChapters(), _loadJobs()]);
  }

  Future<void> _loadJobs({bool silent = false}) async {
    final list = await NasClient.instance.comicJobs(
      sourceKey: widget.sourceKey,
      comicId: widget.comicId,
    );
    if (!mounted) return;
    setState(() {
      _jobs = list ?? const [];
    });
    if (!silent && list == null) {
      // 任务接口失败不影响章节展示，静默即可
    }
  }

  Future<void> _loadChapters() async {
    setState(() {
      _loading = true;
      _error = null;
    });
    final list = await NasClient.instance.comicChapters(
      sourceKey: widget.sourceKey,
      comicId: widget.comicId,
    );
    if (!mounted) return;
    setState(() {
      _chapters = list;
      _loading = false;
      if (list == null) {
        _error = "NAS is unreachable. Check the connection.".tl;
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 16, 16, 8),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                const Icon(Icons.cloud_outlined, size: 20),
                const SizedBox(width: 8),
                Expanded(
                  child: Text(
                    "NAS Storage".tl,
                    style: const TextStyle(
                        fontSize: 16, fontWeight: FontWeight.w600),
                  ),
                ),
                IconButton(
                  icon: const Icon(Icons.refresh, size: 20),
                  onPressed: _loadChapters,
                ),
              ],
            ),
            Text(
              widget.comicTitle,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: TextStyle(
                fontSize: 12,
                color: theme.colorScheme.onSurfaceVariant,
              ),
            ),
            const SizedBox(height: 12),
            // ---- 这本漫画的下载任务（实时进度）----
            // 放在章节清单**之前**：用户打开面板最想确认的是
            // "我刚点的下载到底在跑没有、跑到哪了"，
            // 已缓存的章节是静态结果，放后面。
            if (_activeJobs.isNotEmpty) ...[
              _buildJobsSection(theme),
              const Divider(),
            ],
            if (_loading)
              const Padding(
                padding: EdgeInsets.symmetric(vertical: 32),
                child: Center(child: CircularProgressIndicator()),
              )
            else if (_error != null)
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 24),
                child: Center(child: Text(_error!)),
              )
            else if ((_chapters ?? []).isEmpty)
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 24),
                child: Center(child: Text("Not cached on NAS".tl)),
              )
            else ...[
              _buildSummary(theme),
              const Divider(),
              ConstrainedBox(
                constraints: const BoxConstraints(maxHeight: 340),
                child: ListView.separated(
                  shrinkWrap: true,
                  itemCount: _chapters!.length,
                  separatorBuilder: (_, __) => const Divider(height: 1),
                  itemBuilder: (context, i) => _buildRow(_chapters![i]),
                ),
              ),
              const SizedBox(height: 8),
              TextButton.icon(
                icon: const Icon(Icons.delete_sweep_outlined, size: 18),
                label: Text("Delete all cached chapters of this comic".tl),
                onPressed: _deleteAll,
              ),
            ],
          ],
        ),
      ),
    );
  }

  /// 需要展示的任务：进行中/排队/失败/暂停。
  ///
  /// 已完成的排除掉——它的结果已经体现在下面的"已缓存章节"里，
  /// 再列一遍只会让面板变长。
  List<Map<String, dynamic>> get _activeJobs => _jobs.where((j) {
        final st = j['status']?.toString() ?? '';
        return st == 'running' || st == 'pending' ||
            st == 'paused' || st == 'failed';
      }).toList();

  static String _fmtSpeed(double bps) {
    if (bps <= 0) return '';
    const units = ['B/s', 'KB/s', 'MB/s'];
    var v = bps;
    var i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return '${v.toStringAsFixed(v >= 100 || i == 0 ? 0 : 1)} ${units[i]}';
  }

  /// 任务区块：每条任务一行，带实时进度条 / 页数 / 速度 / 取消按钮。
  Widget _buildJobsSection(ThemeData theme) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          children: [
            Icon(Icons.downloading, size: 16, color: theme.colorScheme.primary),
            const SizedBox(width: 6),
            Text(
              "Download tasks".tl,
              style: const TextStyle(fontSize: 13, fontWeight: FontWeight.w600),
            ),
            const SizedBox(width: 6),
            Text(
              '(${_activeJobs.length})',
              style: TextStyle(
                fontSize: 12,
                color: theme.colorScheme.onSurfaceVariant,
              ),
            ),
          ],
        ),
        const SizedBox(height: 6),
        ConstrainedBox(
          constraints: const BoxConstraints(maxHeight: 220),
          child: ListView.builder(
            shrinkWrap: true,
            itemCount: _activeJobs.length,
            itemBuilder: (_, i) => _buildJobRow(_activeJobs[i], theme),
          ),
        ),
      ],
    );
  }

  Widget _buildJobRow(Map<String, dynamic> job, ThemeData theme) {
    final id = (job['id'] as num?)?.toInt() ?? 0;
    final status = job['status']?.toString() ?? '';
    final live = _live[id];

    // 实时帧优先；数据库里的值是 5 秒检查点
    final progress = live != null && live.status == 'running'
        ? live.progress
        : (job['progress'] as num?)?.toDouble() ?? 0;
    final pct = (progress.clamp(0.0, 1.0) * 100).round();
    final speed = live != null && live.status == 'running'
        ? _fmtSpeed(live.speedBps)
        : '';
    final pages = live != null && live.imageTotal > 0
        ? '${live.imageDone}/${live.imageTotal}'
        : '';
    final message = live?.message.isNotEmpty == true
        ? live!.message
        : (job['message']?.toString() ?? '');
    final error = job['error']?.toString() ?? '';

    late Color color;
    late String label;
    switch (status) {
      case 'running':
        color = theme.colorScheme.primary;
        label = "Downloading".tl;
        break;
      case 'failed':
        color = theme.colorScheme.error;
        label = "Failed".tl;
        break;
      case 'paused':
        color = Colors.orange;
        label = "Paused".tl;
        break;
      default:
        color = Colors.orange;
        label = "Queued".tl;
    }

    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 5),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              Text(label,
                  style: TextStyle(
                      fontSize: 12, color: color, fontWeight: FontWeight.w600)),
              const SizedBox(width: 8),
              if (status == 'running') ...[
                Text('$pct%',
                    style: TextStyle(
                        fontSize: 12,
                        fontWeight: FontWeight.w600,
                        color: color)),
                if (pages.isNotEmpty) ...[
                  const SizedBox(width: 6),
                  Text('$pages ${"pages".tl}',
                      style: TextStyle(
                          fontSize: 11,
                          color: theme.colorScheme.onSurfaceVariant)),
                ],
                if (speed.isNotEmpty) ...[
                  const SizedBox(width: 6),
                  Text(speed,
                      style: TextStyle(
                          fontSize: 11,
                          color: theme.colorScheme.onSurfaceVariant)),
                ],
              ],
              const Spacer(),
              if (status == 'running' ||
                  status == 'pending' ||
                  status == 'paused')
                SizedBox(
                  height: 26,
                  child: TextButton(
                    style: TextButton.styleFrom(
                      padding: const EdgeInsets.symmetric(horizontal: 8),
                      minimumSize: Size.zero,
                      tapTargetSize: MaterialTapTargetSize.shrinkWrap,
                    ),
                    onPressed: () async {
                      final ok = await NasClient.instance.cancelJob(id);
                      if (!mounted) return;
                      if (ok) {
                        context.showMessage(message: "Cancelled".tl);
                        _loadJobs(silent: true);
                      }
                    },
                    child: Text("Cancel".tl,
                        style: TextStyle(
                            fontSize: 12, color: theme.colorScheme.error)),
                  ),
                ),
            ],
          ),
          if (status == 'running' || status == 'pending')
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: ClipRRect(
                borderRadius: BorderRadius.circular(2),
                child: LinearProgressIndicator(
                  value: status == 'running' ? progress.clamp(0.0, 1.0) : null,
                  minHeight: 4,
                ),
              ),
            ),
          if (message.isNotEmpty && status != 'failed')
            Padding(
              padding: const EdgeInsets.only(top: 2),
              child: Text(message,
                  maxLines: 1,
                  overflow: TextOverflow.ellipsis,
                  style: TextStyle(
                      fontSize: 11,
                      color: theme.colorScheme.onSurfaceVariant)),
            ),
          if (error.isNotEmpty)
            Padding(
              padding: const EdgeInsets.only(top: 2),
              child: Text(error,
                  maxLines: 2,
                  overflow: TextOverflow.ellipsis,
                  style: TextStyle(
                      fontSize: 11, color: theme.colorScheme.error)),
            ),
        ],
      ),
    );
  }

  Widget _buildSummary(ThemeData theme) {    final chapters = _chapters ?? [];
    var bytes = 0;
    var pages = 0;
    for (final c in chapters) {
      bytes += (c['bytes'] as num?)?.toInt() ?? 0;
      pages += (c['pages'] as num?)?.toInt() ?? 0;
    }
    return Row(
      children: [
        Text(
          "${chapters.length} ${"Chapters".tl} · $pages ${"Pages".tl} · "
          "${_fmtBytes(bytes)}",
          style: TextStyle(
            fontSize: 12,
            color: theme.colorScheme.onSurfaceVariant,
          ),
        ),
      ],
    );
  }

  Widget _buildRow(Map<String, dynamic> ch) {
    final id = ch['chapterId']?.toString() ?? '';
    final title = ch['title']?.toString() ?? id;
    final pages = (ch['pages'] as num?)?.toInt() ?? 0;
    final bytes = (ch['bytes'] as num?)?.toInt() ?? 0;
    final deleting = _deleting.contains(id);

    return ListTile(
      dense: true,
      contentPadding: EdgeInsets.zero,
      title: Text(
        title.isEmpty ? id : title,
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
        style: const TextStyle(fontSize: 13),
      ),
      subtitle: Text(
        '$pages ${"Pages".tl} · ${_fmtBytes(bytes)}',
        style: const TextStyle(fontSize: 11),
      ),
      trailing: deleting
          ? const SizedBox(
              width: 16,
              height: 16,
              child: CircularProgressIndicator(strokeWidth: 2),
            )
          : IconButton(
              icon: const Icon(Icons.delete_outline, size: 20),
              onPressed: () => _deleteOne(id),
            ),
    );
  }

  Future<void> _deleteOne(String chapterId) async {
    setState(() => _deleting.add(chapterId));
    final ok = await widget.cache.deleteChapter(chapterId);
    if (!mounted) return;
    setState(() => _deleting.remove(chapterId));
    if (ok) {
      await _loadChapters();
    } else if (mounted) {
      context.showMessage(message: "Failed to delete from NAS".tl);
    }
  }

  Future<void> _deleteAll() async {
    // showConfirmDialog 是回调式（不是返回 bool），这里用 Completer 桥接
    final completer = Completer<bool>();
    showConfirmDialog(
      context: context,
      title: "Delete all cached chapters of this comic".tl,
      content: widget.comicTitle,
      onConfirm: () {
        if (!completer.isCompleted) completer.complete(true);
      },
      btnColor: Theme.of(context).colorScheme.error,
    ).then((_) {
      if (!completer.isCompleted) completer.complete(false);
    });

    final ok = await completer.future;
    if (!ok || !mounted) return;

    final done = await widget.cache.deleteComic();
    if (!mounted) return;
    context.showMessage(
      message: done ? "Deleted from NAS".tl : "Failed to delete from NAS".tl,
    );
    if (done) {
      NasChapterCache.invalidate(widget.sourceKey, widget.comicId);
      await _loadChapters();
    }
  }

  static String _fmtBytes(int b) {
    if (b <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    double v = b.toDouble();
    int i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return '${v.toStringAsFixed(v >= 100 || i == 0 ? 0 : 1)}${units[i]}';
  }
}
