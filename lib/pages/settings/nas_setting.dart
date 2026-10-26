part of 'settings_page.dart';

/// NAS 服务端设置页。
///
/// 交互设计与 WebDAV 设置保持一致：
///   - 填地址 + Token，点 Continue 时立即探测
///   - 探测失败保留原配置，避免用户把能用的配置改坏
///   - 额外提供"手动同步"与"查看服务端状态"
class NasSetting extends StatefulWidget {
  const NasSetting({super.key});

  @override
  State<NasSetting> createState() => _NasSettingState();
}

class _NasSettingState extends State<NasSetting> {
  late TextEditingController _urlController;
  late TextEditingController _tokenController;

  bool isTesting = false;
  bool isSyncing = false;
  bool isSyncingSources = false;

  /// 服务端探测结果，用于展示（null 表示还未探测）
  Map<String, dynamic>? serverInfo;
  String? probeError;

  @override
  void initState() {
    super.initState();
    var cfg = NasClient.instance.config;
    _urlController = TextEditingController(text: cfg.baseUrl);
    _tokenController = TextEditingController(text: cfg.token);
    if (cfg.isValid) {
      _refreshInfo();
    }
  }

  @override
  void dispose() {
    _urlController.dispose();
    _tokenController.dispose();
    super.dispose();
  }

  Future<void> _refreshInfo() async {
    var info = await NasClient.instance.info();
    if (!mounted) return;
    setState(() {
      serverInfo = info;
    });
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final cfg = NasClient.instance.config;
    final enabled = cfg.isValid;

    return PopUpWidgetScaffold(
      title: "NAS Server".tl,
      body: SingleChildScrollView(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const SizedBox(height: 12),

            // ---- 状态卡片 ----
            _StatusCard(
              status: NasClient.instance.status,
              serverInfo: serverInfo,
            ),
            const SizedBox(height: 16),

            // ---- 地址 ----
            TextField(
              decoration: InputDecoration(
                labelText: "Server URL".tl,
                hintText: "http://192.168.1.10:8080",
                helperText: "The venera-nas service address".tl,
                border: const OutlineInputBorder(),
              ),
              controller: _urlController,
              keyboardType: TextInputType.url,
            ),
            const SizedBox(height: 12),

            // ---- Token ----
            TextField(
              decoration: InputDecoration(
                labelText: "Access Token (Optional)".tl,
                hintText: "Bearer token configured on the server".tl,
                helperText: "Leave empty if the server does not require auth".tl,
                border: const OutlineInputBorder(),
              ),
              controller: _tokenController,
            ),
            const SizedBox(height: 16),

            if (probeError != null)
              Container(
                padding: const EdgeInsets.all(12),
                decoration: BoxDecoration(
                  color: theme.colorScheme.errorContainer,
                  borderRadius: BorderRadius.circular(8),
                ),
                child: Row(
                  children: [
                    Icon(Icons.error_outline,
                        size: 20, color: theme.colorScheme.onErrorContainer),
                    const SizedBox(width: 8),
                    Expanded(
                      child: Text(
                        probeError!,
                        style: TextStyle(color: theme.colorScheme.onErrorContainer),
                      ),
                    ),
                  ],
                ),
              ),

            const SizedBox(height: 16),

            // ---- 说明 ----
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: theme.colorScheme.surfaceContainerHighest,
                borderRadius: BorderRadius.circular(8),
              ),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text("What NAS mode does".tl,
                      style: const TextStyle(fontWeight: FontWeight.bold)),
                  const SizedBox(height: 6),
                  _bullet("Favorites and reading progress sync across devices"
                      .tl),
                  _bullet("The server downloads your favorites automatically".tl),
                  _bullet("Cached chapters are read from NAS first".tl),
                ],
              ),
            ),

            const SizedBox(height: 20),

            // ---- 主按钮 ----
            Center(
              child: Button.filled(
                isLoading: isTesting,
                onPressed: _save,
                child: Text("Continue".tl),
              ),
            ),

            // ---- 连接后的操作 ----
            if (enabled) ...[
              const SizedBox(height: 20),
              const Divider(),
              const SizedBox(height: 12),
              Row(
                children: [
                  Expanded(
                    child: Button.outlined(
                      isLoading: isSyncing,
                      onPressed: _syncNow,
                      child: Text("Sync Now".tl),
                    ),
                  ),
                  const SizedBox(width: 12),
                  Expanded(
                    child: Button.outlined(
                      onPressed: _clear,
                      child: Text("Disconnect".tl),
                    ),
                  ),
                ],
              ),
              const SizedBox(height: 12),
              // 源同步：新设备装好 App 后最需要的一步
              Button.outlined(
                isLoading: isSyncingSources,
                onPressed: _syncSources,
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.center,
                  children: [
                    const Icon(Icons.extension_outlined, size: 18),
                    const SizedBox(width: 8),
                    Text("Sync Sources from NAS".tl),
                  ],
                ),
              ),
              const SizedBox(height: 8),
              // 下载管理与库浏览
              Button.outlined(
                onPressed: () {
                  App.rootContext.to(() => const NasDownloadManager());
                },
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.center,
                  children: [
                    const Icon(Icons.download_outlined, size: 18),
                    const SizedBox(width: 8),
                    Text("NAS Downloads".tl),
                  ],
                ),
              ),
              const SizedBox(height: 8),
              Button.outlined(
                onPressed: () {
                  final url = '${NasClient.instance.config.baseUrl}/admin';
                  launchUrlString(url);
                },
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.center,
                  children: [
                    const Icon(Icons.dns_outlined, size: 18),
                    const SizedBox(width: 8),
                    Text("Open NAS admin panel".tl),
                  ],
                ),
              ),
              if (NasSync().lastError != null) ...[
                const SizedBox(height: 12),
                Text(
                  NasSync().lastError!,
                  style: TextStyle(color: theme.colorScheme.error),
                ),
              ],
            ],
          ],
        ).paddingHorizontal(16),
      ),
    );
  }

  Widget _bullet(String text) {
    return Padding(
      padding: const EdgeInsets.only(top: 4),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          const Text("• "),
          Expanded(child: Text(text)),
        ],
      ),
    );
  }

  Future<void> _save() async {
    var url = _urlController.text.trim();
    var token = _tokenController.text.trim();

    // 清空 = 断开
    if (url.isEmpty) {
      await NasClient.instance.clear();
      if (!mounted) return;
      context.showMessage(message: "Disconnected".tl);
      App.rootPop();
      return;
    }

    setState(() {
      isTesting = true;
      probeError = null;
    });

    // 保留原配置以便失败回滚
    var oldConfig = NasClient.instance.config;

    var newConfig = NasConfig(
      baseUrl: NasConfig.normalize(url),
      token: token,
      clientId: NasClient.ensureClientId(),
      cursor: oldConfig.cursor,
    );
    await NasClient.instance.save(newConfig);

    var ok = await NasClient.instance.probe(force: true);
    if (!mounted) return;

    if (!ok) {
      // 回滚，避免用户丢失原本可用的配置
      if (oldConfig.isValid) {
        await NasClient.instance.save(oldConfig);
      } else {
        await NasClient.instance.clear();
      }
      setState(() {
        isTesting = false;
        probeError = "Cannot reach the server. Please check the URL and network.".tl;
      });
      return;
    }

    // 连接成功：做一次全量同步
    setState(() {
      isTesting = false;
    });
    await _refreshInfo();
    if (!mounted) return;
    context.showMessage(message: "Connected".tl);

    // 关键顺序：**先**补源并启动同步，再关闭页面。
    //
    // 反过来的写法（先 App.rootPop() 再 NasSync().syncNow()）有一个隐蔽的
    // 致命问题：rootPop 在某些路由栈状态下会抛异常，一旦抛出，
    // 后面的同步就永远不执行——现象是"显示已连接，但收藏一条都同步不过来"，
    // 而界面上完全看不出错误。
    //
    // 补源必须排在最前：新设备一个源都没有，收藏落地了也显示不出来。
    final syncFuture = () async {
      await NasSourceSync.syncFromNas();
      await NasSync().syncNow(full: true);
    }();

    App.rootPop();

    // pop 之后仍保留一份引用，避免 future 被 GC 前无人等待；
    // 失败已经在内部记录，这里只兜住未捕获异常。
    unawaited(syncFuture.catchError((Object e) {
      Log.error('NAS Sync', 'post-connect sync failed: $e');
    }));
  }

  Future<void> _syncNow() async {
    setState(() {
      isSyncing = true;
    });
    await NasSync().syncNow(full: false);
    if (!mounted) return;
    setState(() {
      isSyncing = false;
    });
    var sync = NasSync();
    if (sync.lastError == null) {
      context.showMessage(message: "Synced".tl);
    } else {
      context.showMessage(message: sync.lastError!);
    }
    await _refreshInfo();
  }

  Future<void> _clear() async {
    await NasClient.instance.clear();
    if (!mounted) return;
    context.showMessage(message: "Disconnected".tl);
    App.rootPop();
  }

  /// 从 NAS 补齐漫画源。
  ///
  /// 强制模式（force: true）：连本机已有的同名源也覆盖。
  /// 之所以给这个选项，是因为"我明明连上了但源还是旧的"是常见诉求，
  /// 而自动流程为了不降级用户本机的新版只补缺失。
  Future<void> _syncSources() async {
    setState(() {
      isSyncingSources = true;
    });
    final before = ComicSourceManager().all().length;
    final added = await NasSourceSync.syncFromNas(force: true);
    final after = ComicSourceManager().all().length;
    if (!mounted) return;
    setState(() {
      isSyncingSources = false;
    });

    if (added == 0 && after == before) {
      context.showMessage(message: "No new sources from NAS".tl);
    } else {
      context.showMessage(
        message: "${"Synced".tl} ${after - before >= 0 ? '+' : ''}"
            "$after",
      );
    }
    await _refreshInfo();
  }
}

/// 顶部状态卡片：展示连接状态与服务端概要。
class _StatusCard extends StatelessWidget {
  const _StatusCard({required this.status, this.serverInfo});

  final NasStatus status;
  final Map<String, dynamic>? serverInfo;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);

    late Color color;
    late IconData icon;
    late String label;

    switch (status) {
      case NasStatus.online:
        color = Colors.green;
        icon = Icons.cloud_done_outlined;
        label = "Online".tl;
        break;
      case NasStatus.offline:
        color = Colors.orange;
        icon = Icons.cloud_off_outlined;
        label = "Offline".tl;
        break;
      case NasStatus.disabled:
        color = theme.colorScheme.outline;
        icon = Icons.cloud_queue_outlined;
        label = "Not configured".tl;
        break;
    }

    return Container(
      padding: const EdgeInsets.all(14),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: color.withValues(alpha: 0.35)),
      ),
      child: Row(
        children: [
          Icon(icon, color: color, size: 28),
          const SizedBox(width: 12),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(label,
                    style: TextStyle(
                        color: color, fontWeight: FontWeight.bold, fontSize: 15)),
                if (serverInfo != null) ...[
                  const SizedBox(height: 4),
                  Text(
                    _summary(serverInfo!),
                    style: TextStyle(
                      fontSize: 12,
                      color: theme.colorScheme.onSurfaceVariant,
                    ),
                  ),
                ],
              ],
            ),
          ),
        ],
      ),
    );
  }

  String _summary(Map<String, dynamic> info) {
    var parts = <String>[];
    if (info['version'] != null) parts.add("v${info['version']}");
    if (info['sourceCount'] != null) {
      parts.add("${info['sourceCount']} sources");
    }
    var bytes = (info['libraryBytes'] as num?)?.toInt() ?? 0;
    if (bytes > 0) parts.add(_fmtBytes(bytes));
    if (info['webdavEnabled'] == true) parts.add("WebDAV");
    return parts.join(' · ');
  }

  static String _fmtBytes(int b) {
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
