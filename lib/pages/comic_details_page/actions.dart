part of 'comic_page.dart';

abstract mixin class _ComicPageActions {
  void update();

  ComicDetails get comic;

  ComicSource get comicSource => ComicSource.find(comic.sourceKey)!;

  History? get history;

  /// NAS 章节缓存状态（由 _ComicPageState 提供；未启用 NAS 时为 null）。
  NasChapterCache? get nasCache;

  /// 由 _ComicPageState 实现：确保 nasCache 已创建并开始加载。
  ///
  /// 云朵按钮在 buildTitle（早于 buildChapters）就要用到缓存状态，
  /// 而 cache 的创建原本只在 buildChapters 里发生，因此需要这个钩子。
  void ensureNasCache();

  bool isLiking = false;

  bool isLiked = false;

  void likeOrUnlike() async {
    if (isLiking) return;
    isLiking = true;
    update();
    var res = await comicSource.likeOrUnlikeComic!(comic.id, isLiked);
    if (res.error) {
      App.rootContext.showMessage(message: res.errorMessage!);
    } else {
      isLiked = !isLiked;
    }
    isLiking = false;
    update();
  }

  /// whether the comic is added to local favorite
  bool isAddToLocalFav = false;

  /// whether the comic is favorite on the server
  bool isFavorite = false;

  FavoriteItem _toFavoriteItem() {
    var tags = <String>[];
    for (var e in comic.tags.entries) {
      tags.addAll(e.value.map((tag) => '${e.key}:$tag'));
    }
    return FavoriteItem(
      id: comic.id,
      name: comic.title,
      coverPath: comic.cover,
      author: comic.subTitle ?? comic.uploader ?? '',
      type: comic.comicType,
      tags: tags,
    );
  }

  void openFavPanel() {
    showSideBar(
      App.rootContext,
      _FavoritePanel(
        cid: comic.id,
        type: comic.comicType,
        isFavorite: isFavorite,
        onFavorite: (local, network) {
          if (network != null) {
            isFavorite = network;
          }
          if (local != null) {
            isAddToLocalFav = local;
          }
          update();
        },
        favoriteItem: _toFavoriteItem(),
        updateTime: comic.findUpdateTime(),
      ),
    );
  }

  void quickFavorite() {
    var folder = appdata.settings['quickFavorite'];
    if (folder is! String) {
      return;
    }
    var item = _toFavoriteItem();
    LocalFavoritesManager().addComic(
      folder,
      item,
      null,
      comic.findUpdateTime(),
    );
    NasSync().onFavoriteAdded(item);
    isAddToLocalFav = true;
    update();
    App.rootContext.showMessage(message: "Added".tl);
  }

  void share() {
    var text = comic.title;
    if (comic.url != null) {
      text += '\n${comic.url}';
    }
    Share.shareText(text);
  }

  /// read the comic
  ///
  /// [ep] the episode number, start from 1
  ///
  /// [page] the page number, start from 1
  ///
  /// [group] the chapter group number, start from 1
  void read([int? ep, int? page, int? group]) {
    App.rootContext
        .to(
      () => Reader(
        type: comic.comicType,
        cid: comic.id,
        name: comic.title,
        chapters: comic.chapters,
        initialChapter: ep,
        initialPage: page,
        initialChapterGroup: group,
        history: history ?? History.fromModel(model: comic, ep: 0, page: 0),
        author: comic.findAuthor() ?? '',
        tags: comic.plainTags,
      )
    )
        .then((_) {
      onReadEnd();
    });
  }

  void continueRead() {
    var ep = history?.ep ?? 1;
    var page = history?.page ?? 1;
    var group = history?.group ?? 1;
    read(ep, page, group);
  }

  void onReadEnd();

  /// 把这部漫画下载到 NAS（服务端异步执行）。
  ///
  /// 与 [download]（下载到本机）是两条独立路径：NAS 上的副本可以在所有
  /// 设备间共享，本机副本只对当前设备可见。
  ///
  /// **增量语义（关键）**：这里不再自己判断"已缓存就拦住"，而是把判断
  /// 交给服务端。服务端拿源端章节表与已缓存集合求差集，只下缺的章节。
  ///
  /// 旧行为（用户报的问题）：
  ///   ```dart
  ///   if (cached.length >= total) { 提示已缓存; return; }
  ///   ```
  /// 它只按**已缓存章节数 ≥ 章节总数**判断，于是"已缓存 10 章、
  /// 源站更新到 12 章"时 cached=10 < total=12 会放行，但服务端那边
  /// 只要发现有任何缓存就回 409 already cached，后 2 章永远下不到；
  /// 而如果是「源站章节数没变、但内容重传」，cached>=total 又会被这里
  /// 直接拦死。两头都不对。
  Future<void> downloadToNas() async {
    final client = NasClient.instance;
    if (!client.isEnabled) {
      App.rootContext.showMessage(
        message: "NAS is not configured. Set it up in Settings.".tl,
      );
      return;
    }

    // 先探活，给出明确反馈而不是静默失败
    if (!await client.probe(force: true)) {
      App.rootContext.showMessage(
        message: "NAS is unreachable. Check the connection.".tl,
      );
      return;
    }

    final res = await client.requestDownload(
      sourceKey: comic.sourceKey,
      comicId: comic.id,
      title: comic.title,
    );
    if (res == null) {
      App.rootContext.showMessage(message: "Failed to queue NAS download".tl);
      return;
    }

    // 三种结果分别给话术，用户才知道"点了到底干了什么"
    if (res.isUpToDate) {
      App.rootContext.showMessage(
        message: res.total > 0
            ? "${"Already cached on NAS".tl} (${res.total})"
            : "Already cached on NAS".tl,
      );
      return;
    }
    if (res.isAlreadyQueued) {
      App.rootContext.showMessage(message: "Queued on NAS".tl);
      return;
    }

    // 插值不能放进 .tl 的键里（键是字面量，带变量就永远命中不了翻译表）
    final detail = res.total > 0 && res.missing > 0 && res.missing < res.total
        // 增量：明确告诉用户只补了几章，否则他会以为整本重下了
        ? " (${"Missing @n chapters".tlParams({'n': '${res.missing}'})})"
        : "";
    App.rootContext.showMessage(
      message: "${"Queued on NAS".tl}$detail (#${res.jobId})",
    );

    // 缓存状态已变（服务端会陆续写入），让详情页角标尽早重查
    NasChapterCache.invalidate(comic.sourceKey, comic.id);
    nasCache?.load(force: true);

    // 轻量进度反馈：轮询直到完成或失败（避免用户不知道进度）
    _watchNasJob(client, res.jobId!);
  }

  /// 轮询 NAS 任务进度，结束后提示。
  void _watchNasJob(NasClient client, int jobId) async {
    for (var i = 0; i < 120; i++) {
      await Future.delayed(const Duration(seconds: 5));
      final job = await client.getJob(jobId);
      if (job == null) continue;
      final status = job['status']?.toString() ?? '';
      if (status == 'done') {
        // 下载完成后角标必须重查：服务端此刻才真正写入 cached_chapters
        NasChapterCache.invalidate(comic.sourceKey, comic.id);
        nasCache?.load(force: true);
        App.rootContext.showMessage(
          message: "NAS download complete".tl,
        );
        return;
      }
      if (status == 'failed') {
        final reason = job['error']?.toString() ?? '';
        App.rootContext.showMessage(
          message: reason.isEmpty
              ? "NAS download failed".tl
              : "${"NAS download failed".tl}: $reason",
        );
        return;
      }
      if (status == 'cancelled') return;
    }
  }

  void download() async {
    if (LocalManager().isDownloading(comic.id, comic.comicType)) {
      App.rootContext.showMessage(message: "The comic is downloading".tl);
      return;
    }
    if (comic.chapters == null &&
        LocalManager().isDownloaded(comic.id, comic.comicType, 0)) {
      App.rootContext.showMessage(message: "The comic is downloaded".tl);
      return;
    }

    if (comicSource.archiveDownloader != null) {
      bool useNormalDownload = false;
      List<ArchiveInfo>? archives;
      int selected = -1;
      bool isLoading = false;
      bool isGettingLink = false;
      await showDialog(
        context: App.rootContext,
        builder: (context) {
          return StatefulBuilder(
            builder: (context, setState) {
              return ContentDialog(
                title: "Download".tl,
                content: RadioGroup<int>(
                  groupValue: selected,
                  onChanged: (v) {
                    setState(() {
                      selected = v ?? selected;
                    });
                  },
                  child: Column(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      RadioListTile<int>(
                        value: -1,
                        title: Text("Normal".tl),
                      ),
                      ExpansionTile(
                        title: Text("Archive".tl),
                        shape: const RoundedRectangleBorder(
                          borderRadius: BorderRadius.zero,
                        ),
                        collapsedShape: const RoundedRectangleBorder(
                          borderRadius: BorderRadius.zero,
                        ),
                        onExpansionChanged: (b) {
                          if (!isLoading && b && archives == null) {
                            isLoading = true;
                            comicSource.archiveDownloader!
                                .getArchives(comic.id)
                                .then((value) {
                              if (value.success) {
                                archives = value.data;
                              } else {
                                App.rootContext
                                    .showMessage(message: value.errorMessage!);
                              }
                              setState(() {
                                isLoading = false;
                              });
                            });
                          }
                        },
                        children: [
                          if (archives == null)
                            const ListLoadingIndicator().toCenter()
                          else
                            for (int i = 0; i < archives!.length; i++)
                              RadioListTile<int>(
                                value: i,
                                title: Text(archives![i].title),
                                subtitle: Text(archives![i].description),
                              )
                        ],
                      )
                    ],
                  ),
                ),
                actions: [
                  Button.filled(
                    isLoading: isGettingLink,
                    onPressed: () async {
                      if (selected == -1) {
                        useNormalDownload = true;
                        context.pop();
                        return;
                      }
                      setState(() {
                        isGettingLink = true;
                      });
                      var res =
                          await comicSource.archiveDownloader!.getDownloadUrl(
                        comic.id,
                        archives![selected].id,
                      );
                      if (res.error) {
                        App.rootContext.showMessage(message: res.errorMessage!);
                        setState(() {
                          isGettingLink = false;
                        });
                      } else if (context.mounted) {
                        if (res.data.isNotEmpty) {
                          LocalManager()
                            .addTask(ArchiveDownloadTask(res.data, comic));
                          App.rootContext
                            .showMessage(message: "Download started".tl);
                        }
                        context.pop();
                      }
                    },
                    child: Text("Confirm".tl),
                  ),
                ],
              );
            },
          );
        },
      );
      if (!useNormalDownload) {
        return;
      }
    }

    if (comic.chapters == null) {
      LocalManager().addTask(ImagesDownloadTask(
        source: comicSource,
        comicId: comic.id,
        comic: comic,
      ));
    } else {
      List<int>? selected;
      var downloaded = <int>[];
      var localComic = LocalManager().find(comic.id, comic.comicType);
      if (localComic != null) {
        for (int i = 0; i < comic.chapters!.length; i++) {
          if (localComic.downloadedChapters
              .contains(comic.chapters!.ids.elementAt(i))) {
            downloaded.add(i);
          }
        }
      }
      await showSideBar(
        App.rootContext,
        _SelectDownloadChapter(
          comic.chapters!.titles.toList(),
          (v) => selected = v,
          downloaded,
        ),
      );
      if (selected == null) return;
      LocalManager().addTask(ImagesDownloadTask(
        source: comicSource,
        comicId: comic.id,
        comic: comic,
        chapters: selected!.map((i) {
          return comic.chapters!.ids.elementAt(i);
        }).toList(),
      ));
    }
    App.rootContext.showMessage(message: "Download started".tl);
    update();
  }

  void onTapTag(String tag, String namespace) {
    var target = comicSource.handleClickTagEvent?.call(namespace, tag);
    var context = App.mainNavigatorKey!.currentContext!;
    target?.jump(context);
  }

  void showMoreActions() {
    var context = App.rootContext;
    showMenuX(
        context,
        Offset(
          context.width - 16,
          context.padding.top,
        ),
        [
          MenuEntry(
            icon: Icons.copy,
            text: "Copy Title".tl,
            onClick: () {
              Clipboard.setData(ClipboardData(text: comic.title));
              context.showMessage(message: "Copied".tl);
            },
          ),
          MenuEntry(
            icon: Icons.copy_rounded,
            text: "Copy ID".tl,
            onClick: () {
              Clipboard.setData(ClipboardData(text: comic.id));
              context.showMessage(message: "Copied".tl);
            },
          ),
          if (comic.url != null)
            MenuEntry(
              icon: Icons.link,
              text: "Copy URL".tl,
              onClick: () {
                Clipboard.setData(ClipboardData(text: comic.url!));
                context.showMessage(message: "Copied".tl);
              },
            ),
          if (comic.url != null)
            MenuEntry(
              icon: Icons.open_in_browser,
              text: "Open in Browser".tl,
              onClick: () {
                launchUrlString(comic.url!);
              },
            ),
          // ---- NAS 相关 ----
          // 只在启用且在线时出现：离线时这些操作全都会静默失败，
          // 给用户一个点了没反应的菜单项比不给更糟。
          if (NasClient.instance.isEnabled &&
              (nasCache?.isAvailable ?? false)) ...[
            MenuEntry(
              icon: Icons.cloud_download_outlined,
              text: "Download whole comic to NAS".tl,
              onClick: downloadToNas,
            ),
            if (nasCache!.cachedChapters.isNotEmpty)
              MenuEntry(
                icon: Icons.cloud_off_outlined,
                text: "Delete all cached chapters of this comic".tl,
                onClick: () async {
                  final ok = await nasCache!.deleteComic();
                  context.showMessage(
                    message: ok
                        ? "Deleted from NAS".tl
                        : "Failed to delete from NAS".tl,
                  );
                },
              ),
            MenuEntry(
              icon: Icons.dns_outlined,
              text: "Open NAS admin panel".tl,
              onClick: () {
                launchUrlString(
                  '${NasClient.instance.config.baseUrl}/admin',
                );
              },
            ),
          ],
        ]);
  }

  /// 打开 NAS 存储管理面板。
  ///
  /// 用户诉求原话是"详情页添加 NAS 资源管理的按钮，可以删除 NAS 资源"。
  /// 做成独立面板而不是塞进长按菜单：管理是"看清单 + 批量删"的场景，
  /// 需要一个能一次看到全部缓存章节的地方。
  ///
  /// 时序处理：用户可能在首帧（nasCache 尚未建好）就点云朵按钮，
  /// 这里主动建一次并等待探测完成，而不是直接报"不可达"。
  Future<void> showNasManage() async {
    var cache = nasCache;
    if (cache == null) {
      ensureNasCache();
      cache = nasCache;
    }
    if (cache == null) {
      App.rootContext.showMessage(
        message: "NAS is not configured. Set it up in Settings.".tl,
      );
      return;
    }
    // 首次打开时可能还在探测：等一次 load 结果（内部有 memo/超时兜底）
    if (!cache.isAvailable && !cache.isLoading) {
      await cache.load();
    }
    if (!cache.isAvailable) {
      App.rootContext.showMessage(
        message: "NAS is unreachable. Check the connection.".tl,
      );
      return;
    }
    if (!App.rootContext.mounted) return;
    showModalBottomSheet(
      context: App.rootContext,
      isScrollControlled: true,
      builder: (_) => _NasStoragePanel(
        cache: cache!,
        comicTitle: comic.title,
        comicId: comic.id,
        sourceKey: comic.sourceKey,
      ),
    );
  }

  void showComments() {
    showSideBar(
      App.rootContext,
      CommentsPage(
        data: comic,
        source: comicSource,
      ),
    );
  }

  void starRating() {
    if (!comicSource.isLogged) {
      return;
    }
    var rating = 0.0;
    var isLoading = false;
    showDialog(
      context: App.rootContext,
      builder: (dialogContext) => StatefulBuilder(
        builder: (context, setState) => SimpleDialog(
          title: const Text("Rating"),
          alignment: Alignment.center,
          children: [
            SizedBox(
              height: 100,
              child: Center(
                child: SizedBox(
                  width: 210,
                  child: Column(
                    children: [
                      const SizedBox(
                        height: 10,
                      ),
                      RatingWidget(
                        padding: 2,
                        onRatingUpdate: (value) => rating = value,
                        value: 1,
                        selectable: true,
                        size: 40,
                      ),
                      const Spacer(),
                      Button.filled(
                        isLoading: isLoading,
                        onPressed: () {
                          setState(() {
                            isLoading = true;
                          });
                          comicSource.starRatingFunc!(comic.id, rating.round())
                              .then((value) {
                            if (value.success) {
                              App.rootContext
                                  .showMessage(message: "Success".tl);
                              Navigator.of(dialogContext).pop();
                            } else {
                              App.rootContext
                                  .showMessage(message: value.errorMessage!);
                              setState(() {
                                isLoading = false;
                              });
                            }
                          });
                        },
                        child: Text("Submit".tl),
                      )
                    ],
                  ),
                ),
              ),
            )
          ],
        ),
      ),
    );
  }
}
