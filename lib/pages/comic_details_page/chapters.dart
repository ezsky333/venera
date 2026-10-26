part of 'comic_page.dart';

class _ComicChapters extends StatelessWidget {
  const _ComicChapters({
    this.history,
    required this.groupedMode,
    this.nasCache,
  });

  final History? history;

  final bool groupedMode;

  /// NAS 章节缓存状态；为空表示未启用 NAS 或尚未就绪。
  final NasChapterCache? nasCache;

  @override
  Widget build(BuildContext context) {
    return groupedMode
        ? _GroupedComicChapters(history, nasCache)
        : _NormalComicChapters(history, nasCache);
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
  const _NormalComicChapters(this.history, this.nasCache);

  final History? history;

  final NasChapterCache? nasCache;

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
                  final cached = nas?.cachedChapters.contains(key) ?? false;
                  final requesting = nas?.isRequesting(key) ?? false;
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
                                    color: visited
                                        ? context.colorScheme.outline
                                        : null,
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
  const _GroupedComicChapters(this.history, this.nasCache);

  final History? history;

  final NasChapterCache? nasCache;

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
                  final cached = nas?.cachedChapters.contains(key) ?? false;
                  final requesting = nas?.isRequesting(key) ?? false;
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
                                    color: visited
                                        ? context.colorScheme.outline
                                        : null,
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
              final ok = await cache.requestDownload(chapters: [chapterId]);
              if (!context.mounted) return;
              context.showMessage(
                message: ok
                    ? "Queued download to NAS".tl
                    : "Failed to request NAS download".tl,
              );
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
