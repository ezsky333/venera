import 'dart:async';

import 'package:flutter/material.dart';

import 'package:venera/components/components.dart';
import 'package:venera/foundation/app.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/utils/translations.dart';

/// NAS 下载管理页。
///
/// 与「本地下载」页并列的另一条路径：本地下载把图片存到手机，
/// NAS 下载把图片存到服务端（所有设备共享）。两者互不影响，
/// 因此这里刻意做成独立页面，避免混淆「我删了本地但 NAS 还在」。
///
/// 能力对齐 Web 管理面板（/admin 的「任务」标签页）：
///   - 单条：暂停 / 继续 / 置顶 / 取消 / 重试 / 删除
///   - 批量：全部暂停 / 全部继续 / 重试失败 / 清理已完成 / 补录未缓存收藏
///   - 筛选 + 分页 + 顶部总览
///
/// 为什么要把这些搬进 App：NAS 往往就跑在家里，用户人在外面只有手机，
/// 打开浏览器输地址 + token 才能停掉一个跑飞的队列，门槛太高。
///
/// 实时性设计（与 Web 面板一致）：
///   - 通过 SSE (`/api/v1/jobs/events`) 订阅服务端推送；
///   - `live` 帧只更新进度条/速度/页数，**不重建整个列表**，
///     所以进度是连续流动的，而不是每 5 秒跳一格；
///   - `jobs` 帧（状态变化时才推）才刷新列表结构；
///   - SSE 不可用时自动退回 5 秒轮询，功能不降级。
class NasDownloadManager extends StatefulWidget {
  const NasDownloadManager({super.key});

  @override
  State<NasDownloadManager> createState() => _NasDownloadManagerState();
}

class _NasDownloadManagerState extends State<NasDownloadManager> {
  List<Map<String, dynamic>> _jobs = [];
  bool _loading = true;
  String? _error;

  /// 当前筛选的状态（空表示全部）。
  String _filter = '';

  /// 分页。
  static const _pageSize = 50;
  int _offset = 0;
  int _total = 0;

  /// 顶部总览（与 Web 面板同一份 stats）。
  Map<String, dynamic>? _stats;

  /// jobId → 实时状态（来自 SSE live 帧）。
  ///
  /// 单独存放而不是合并进 _jobs，是为了让 progress 变化时
  /// 只触发进度条重绘，不重建整行 Widget。
  Map<int, NasJobLive> _live = {};

  /// 正在执行操作的 jobId（行内按钮转圈，避免连点）。
  final Set<int> _busy = {};

  NasJobEventStream? _events;
  StreamSubscription<NasJobEvent>? _eventSub;
  Timer? _fallbackPoll;
  bool _sseAlive = false;

  @override
  void initState() {
    super.initState();
    _load();
    _connectEvents();
  }

  @override
  void dispose() {
    _eventSub?.cancel();
    _events?.dispose();
    _fallbackPoll?.cancel();
    super.dispose();
  }

  /// 建立 SSE 订阅；失败时启用轮询兜底。
  void _connectEvents() {
    if (!NasClient.instance.isEnabled) return;
    _events?.dispose();
    _events = NasClient.instance.subscribeJobs(
      status: _filter.isEmpty ? null : _filter,
    );
    _eventSub?.cancel();
    _eventSub = _events!.stream.listen(
      _onEvent,
      onError: (_) => _startFallbackPoll(),
      onDone: _startFallbackPoll,
    );
    _events!.connect();
  }

  void _onEvent(NasJobEvent ev) {
    if (!mounted) return;
    _sseAlive = true;
    _fallbackPoll?.cancel();
    _fallbackPoll = null;

    if (ev.live.isNotEmpty) {
      setState(() {
        _live = {for (final l in ev.live) l.jobId: l};
      });
      return;
    }
    if (ev.jobs.isNotEmpty || ev.live.isEmpty) {
      // 列表快照：结构变化，整体替换
      setState(() {
        _jobs = ev.jobs;
        _total = ev.jobs.length > _total ? ev.jobs.length : _total;
        _loading = false;
        _error = null;
        // 清理已不在列表里的实时状态，避免内存增长
        final ids = ev.jobs.map((j) => (j['id'] as num?)?.toInt() ?? 0).toSet();
        _live.removeWhere((k, _) => !ids.contains(k));
      });
      // 状态跃变时顺带刷新总览，让"失败 3 / 完成 12"跟着走
      _loadStats();
    }
  }

  /// SSE 不可用时的兜底：5 秒轮询。
  void _startFallbackPoll() {
    if (!mounted || _fallbackPoll != null) return;
    _sseAlive = false;
    _fallbackPoll = Timer.periodic(const Duration(seconds: 5), (_) {
      if (mounted && !_loading) _load(silent: true);
    });
  }

  Future<void> _load({bool silent = false}) async {
    if (!silent) {
      setState(() {
        _loading = true;
        _error = null;
      });
    }
    if (!NasClient.instance.isEnabled) {
      setState(() {
        _loading = false;
        _error = "NAS is not configured. Set it up in Settings.".tl;
      });
      return;
    }
    final ok = await NasClient.instance.probe();
    if (!ok) {
      setState(() {
        _loading = false;
        _error = "NAS is unreachable. Check the connection.".tl;
      });
      return;
    }
    final page = await NasClient.instance.jobsPage(
      status: _filter.isEmpty ? null : _filter,
      limit: _pageSize,
      offset: _offset,
    );
    if (!mounted) return;
    setState(() {
      _jobs = page.jobs;
      // 服务端 total 才是"该筛选下共有多少条"，用它画分页；
      // 但 SSE 推来的 jobs 帧只带一页列表，别把 total 冲掉。
      if (page.total > 0) _total = page.total;
      _loading = false;
      _error = null;
    });
    unawaited(_loadStats());
  }

  Future<void> _loadStats() async {
    final s = await NasClient.instance.stats();
    if (!mounted || s == null) return;
    setState(() => _stats = s);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: Text("NAS Downloads".tl),
        actions: [
          Padding(
            padding: const EdgeInsets.only(right: 4),
            child: Center(
              child: Tooltip(
                message: _sseAlive ? "Live".tl : "Polling".tl,
                child: Icon(
                  _sseAlive ? Icons.bolt : Icons.sync,
                  size: 18,
                  color: _sseAlive
                      ? Theme.of(context).colorScheme.primary
                      : Theme.of(context).colorScheme.outline,
                ),
              ),
            ),
          ),
          PopupMenuButton<String>(
            icon: const Icon(Icons.more_vert),
            tooltip: "Manage".tl,
            onSelected: _onMenuAction,
            itemBuilder: (context) => [
              PopupMenuItem(
                value: 'pause-all',
                child: ListTile(
                  dense: true,
                  contentPadding: EdgeInsets.zero,
                  leading: const Icon(Icons.pause_circle_outline),
                  title: Text("Pause all".tl),
                ),
              ),
              PopupMenuItem(
                value: 'resume-all',
                child: ListTile(
                  dense: true,
                  contentPadding: EdgeInsets.zero,
                  leading: const Icon(Icons.play_circle_outline),
                  title: Text("Resume all".tl),
                ),
              ),
              PopupMenuItem(
                value: 'retry-failed',
                child: ListTile(
                  dense: true,
                  contentPadding: EdgeInsets.zero,
                  leading: const Icon(Icons.refresh),
                  title: Text("Retry all failed".tl),
                ),
              ),
              PopupMenuItem(
                value: 'backfill',
                child: ListTile(
                  dense: true,
                  contentPadding: EdgeInsets.zero,
                  leading: const Icon(Icons.playlist_add_check),
                  title: Text("Backfill missing favorites".tl),
                ),
              ),
              PopupMenuItem(
                value: 'delete-finished',
                child: ListTile(
                  dense: true,
                  contentPadding: EdgeInsets.zero,
                  leading: Icon(Icons.delete_sweep_outlined,
                      color: Theme.of(context).colorScheme.error),
                  title: Text("Clear finished tasks".tl),
                ),
              ),
            ],
          ),
        ],
      ),
      body: Column(
        children: [
          if (_stats != null) _buildStatsBar(),
          _buildFilterBar(),
          if (_error != null)
            Expanded(child: NetworkError(message: _error!, retry: _load))
          else if (_loading)
            const Expanded(child: Center(child: CircularProgressIndicator()))
          else if (_jobs.isEmpty)
            Expanded(child: Center(child: Text("No NAS downloads".tl)))
          else
            Expanded(
              child: RefreshIndicator(
                onRefresh: () => _load(silent: true),
                child: ListView.separated(
                  itemCount: _jobs.length,
                  separatorBuilder: (_, __) => const Divider(height: 1),
                  itemBuilder: (context, i) => _buildJobRow(_jobs[i]),
                ),
              ),
            ),
          if (_total > _pageSize) _buildPager(),
        ],
      ),
    );
  }

  /// 分页条。
  ///
  /// 之前 App 里一次拉 200 条且不能翻页，任务上千条时后面的根本看不到；
  /// Web 面板早就有分页，这里补齐。
  Widget _buildPager() {
    final theme = Theme.of(context);
    final from = _offset + 1;
    final to = (_offset + _pageSize).clamp(0, _total);
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
      decoration: BoxDecoration(
        border: Border(
          top: BorderSide(color: theme.colorScheme.outlineVariant, width: 0.6),
        ),
      ),
      child: Row(
        children: [
          TextButton(
            onPressed: _offset == 0
                ? null
                : () {
                    setState(() => _offset =
                        (_offset - _pageSize).clamp(0, _total));
                    _load(silent: true);
                  },
            child: Text("Prev".tl),
          ),
          Expanded(
            child: Text(
              '$from–$to / $_total',
              textAlign: TextAlign.center,
              style: TextStyle(
                fontSize: 12,
                color: theme.colorScheme.onSurfaceVariant,
              ),
            ),
          ),
          TextButton(
            onPressed: _offset + _pageSize >= _total
                ? null
                : () {
                    setState(() => _offset += _pageSize);
                    _load(silent: true);
                  },
            child: Text("Next".tl),
          ),
        ],
      ),
    );
  }

  /// 顶部总览条：直接在手机上看到"库里多少本、队列里多少、失败多少"，
  /// 不用再去浏览器看面板。
  Widget _buildStatsBar() {
    final s = _stats!;
    int v(String k) => (s[k] as num?)?.toInt() ?? 0;
    final theme = Theme.of(context);
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
      color: theme.colorScheme.surfaceContainerHighest.withValues(alpha: 0.4),
      child: Wrap(
        spacing: 14,
        runSpacing: 4,
        children: [
          _statItem("Library".tl, '${v('libraryComics')} ${"comics".tl}'),
          _statItem("Queued".tl, '${v('jobsPending')}'),
          _statItem("Downloading".tl, '${v('jobsRunning')}',
              color: theme.colorScheme.primary),
          _statItem("Failed".tl, '${v('jobsFailed')}',
              color: v('jobsFailed') > 0 ? theme.colorScheme.error : null),
          _statItem("Completed".tl, '${v('jobsDone')}'),
        ],
      ),
    );
  }

  Widget _statItem(String label, String value, {Color? color}) {
    final theme = Theme.of(context);
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Text('$label ',
            style: TextStyle(
                fontSize: 11, color: theme.colorScheme.onSurfaceVariant)),
        Text(value,
            style: TextStyle(
                fontSize: 11,
                fontWeight: FontWeight.w600,
                color: color ?? theme.colorScheme.onSurface)),
      ],
    );
  }

  Widget _buildFilterBar() {
    const filters = [
      ['', 'All'],
      ['running', 'Downloading'],
      ['pending', 'Queued'],
      ['paused', 'Paused'],
      ['done', 'Completed'],
      ['failed', 'Failed'],
    ];
    return SingleChildScrollView(
      scrollDirection: Axis.horizontal,
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
      child: Row(
        children: [
          for (final f in filters)
            Padding(
              padding: const EdgeInsets.only(right: 8),
              child: FilterChip(
                label: Text(f[1].tl),
                selected: _filter == f[0],
                onSelected: (_) {
                  setState(() {
                    _filter = f[0];
                    _offset = 0;
                  });
                  // 筛选变了要重新订阅（服务端按 status 过滤 jobs 帧）
                  _connectEvents();
                  _load();
                },
              ),
            ),
          Padding(
            padding: const EdgeInsets.only(right: 8),
            child: IconButton(
              icon: const Icon(Icons.refresh, size: 20),
              tooltip: "Refresh".tl,
              onPressed: () => _load(),
            ),
          ),
        ],
      ),
    );
  }

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

  Widget _buildJobRow(Map<String, dynamic> job) {
    final id = (job['id'] as num?)?.toInt() ?? 0;
    final live = _live[id];
    final isBusy = _busy.contains(id);

    final status = job['status']?.toString() ?? '';
    // 实时帧优先：SSE 还没到或该任务没在跑时用数据库里的值
    final progress = live != null && live.status == 'running'
        ? live.progress
        : (job['progress'] as num?)?.toDouble() ?? 0;
    final title = job['title']?.toString().trim().isNotEmpty == true
        ? job['title'].toString()
        : (job['comicId']?.toString() ?? '');
    final source = job['source']?.toString() ?? '';
    final message = live?.message.isNotEmpty == true
        ? live!.message
        : (job['message']?.toString() ?? '');
    final error = job['error']?.toString() ?? '';
    final priority = (job['priority'] as num?)?.toInt() ?? 0;

    late Color color;
    late String label;
    switch (status) {
      case 'done':
        color = Colors.green;
        label = "Completed".tl;
        break;
      case 'failed':
        color = Colors.red;
        label = "Failed".tl;
        break;
      case 'running':
        color = Theme.of(context).colorScheme.primary;
        label = "Downloading".tl;
        break;
      case 'paused':
        color = Colors.orange;
        label = "Paused".tl;
        break;
      case 'cancelled':
        color = Theme.of(context).colorScheme.outline;
        label = "Cancelled".tl;
        break;
      default:
        color = Colors.orange;
        label = "Queued".tl;
    }

    final isActive = status == 'running' || status == 'pending';
    final pct = (progress.clamp(0.0, 1.0) * 100).round();
    final speed = live != null && live.status == 'running'
        ? _fmtSpeed(live.speedBps)
        : '';
    final pages = live != null && live.imageTotal > 0
        ? '${live.imageDone}/${live.imageTotal}'
        : '';

    return ListTile(
      leading: Icon(
        status == 'done'
            ? Icons.cloud_done
            : status == 'failed'
                ? Icons.cloud_off
                : Icons.cloud_download,
        color: color,
      ),
      title: Row(
        children: [
          if (priority > 0)
            const Padding(
              padding: EdgeInsets.only(right: 4),
              child: Icon(Icons.push_pin, size: 14),
            ),
          Expanded(
            child: Text(title, maxLines: 1, overflow: TextOverflow.ellipsis),
          ),
        ],
      ),
      subtitle: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          // 这一行曾被撑爆（Flutter 的黄黑 "ROW OVERFLOWED BY N PIXELS"
          // 就是布局溢出警告）：状态 + 百分比 + 页数 + 速度 + 源·编号
          // 全部硬塞进一个 Row，窄屏必然溢出。
          //
          // 现在的结构：
          //   - 第一行：状态 · 百分比 · 页数 · 速度（都可省略，用 Wrap 换行）
          //   - 第二行（右对齐）：source · #id
          Wrap(
            spacing: 6,
            runSpacing: 2,
            crossAxisAlignment: WrapCrossAlignment.center,
            children: [
              Text(label, style: TextStyle(color: color, fontSize: 12)),
              if (status == 'running') ...[
                Text('$pct%',
                    style: TextStyle(
                      fontSize: 12,
                      fontWeight: FontWeight.w600,
                      color: color,
                    )),
                if (pages.isNotEmpty)
                  Text('$pages ${"pages".tl}',
                      style: TextStyle(
                        fontSize: 11,
                        color: Theme.of(context).colorScheme.outline,
                      )),
                if (speed.isNotEmpty)
                  Text(speed,
                      style: TextStyle(
                        fontSize: 11,
                        color: Theme.of(context).colorScheme.outline,
                      )),
              ],
              Text('$source · #$id',
                  style: TextStyle(
                    fontSize: 11,
                    color: Theme.of(context).colorScheme.outline,
                  )),
            ],
          ),
          if (isActive)
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
          if (message.isNotEmpty && status != 'done')
            Padding(
              padding: const EdgeInsets.only(top: 2),
              child: Text(message,
                  maxLines: 1,
                  overflow: TextOverflow.ellipsis,
                  style: TextStyle(
                    fontSize: 11,
                    color: Theme.of(context).colorScheme.outline,
                  )),
            ),
          if (error.isNotEmpty)
            Text(error,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
                style: TextStyle(
                    fontSize: 11,
                    color: Theme.of(context).colorScheme.error)),
        ],
      ),
      // isThreeLine 只是"建议三行"，内容更高时仍会溢出；
      // 用 minVerticalPadding 收一点，交给 ListTile 自适应高度。
      isThreeLine: true,
      minVerticalPadding: 8,
      trailing: isBusy
          ? const SizedBox(
              width: 20,
              height: 20,
              child: CircularProgressIndicator(strokeWidth: 2),
            )
          : _buildRowMenu(id, status, priority),
    );
  }

  /// 每行的操作菜单。与 Web 面板的行内按钮一一对应。
  ///
  /// 这里用菜单而不是把六个按钮摊开：手机宽度放不下，
  /// 而且误触风险高（删除/取消是不可逆的）。
  Widget _buildRowMenu(int id, String status, int priority) {
    final items = <PopupMenuEntry<String>>[];
    final canPause = status == 'pending' || status == 'running';
    final canResume = status == 'paused';
    final canCancel = status == 'pending' || status == 'paused' || status == 'running';
    final canRetry = status == 'failed' || status == 'cancelled';
    // 运行中的任务不能删：worker 靠 jobs 行里的 message 判断该不该停，
    // 行被删掉后它就再也停不下来（会继续把整本下完）。先暂停再删。
    final canDelete = status != 'running';

    if (canPause) {
      items.add(_menuItem('pause', Icons.pause, "Pause".tl));
    }
    if (canResume) {
      items.add(_menuItem('resume', Icons.play_arrow, "Resume".tl));
    }
    if (status == 'pending' || status == 'paused') {
      items.add(_menuItem(
        priority > 0 ? 'untop' : 'top',
        Icons.push_pin_outlined,
        priority > 0 ? "Unpin".tl : "Pin to top".tl,
      ));
    }
    if (canRetry) {
      items.add(_menuItem('retry', Icons.refresh, "Retry".tl));
    }
    if (canCancel) {
      items.add(_menuItem('cancel', Icons.close, "Cancel".tl));
    }
    if (canDelete) {
      items.add(_menuItem('delete', Icons.delete_outline, "Delete".tl,
          danger: true));
    }
    if (items.isEmpty) return const SizedBox.shrink();

    return PopupMenuButton<String>(
      icon: const Icon(Icons.more_vert, size: 20),
      tooltip: "Manage".tl,
      onSelected: (a) => _jobAction(id, a),
      itemBuilder: (_) => items,
    );
  }

  PopupMenuItem<String> _menuItem(
    String value,
    IconData icon,
    String text, {
    bool danger = false,
  }) {
    final color = danger ? Theme.of(context).colorScheme.error : null;
    return PopupMenuItem<String>(
      value: value,
      child: Row(
        children: [
          Icon(icon, size: 18, color: color),
          const SizedBox(width: 10),
          Text(text, style: TextStyle(color: color)),
        ],
      ),
    );
  }

  /// 执行单个任务动作。
  ///
  /// 返回值语义：`!xxx` 前缀表示服务端拒绝了这次操作（例如删除运行中
  /// 的任务），原样显示比"操作失败"有用得多。
  Future<void> _jobAction(int id, String action) async {
    // 删除是不可逆的，先在本地确认一次（服务端的 409 只挡 running）
    if (action == 'delete') {
      final ok = await _confirm(
        "Delete task".tl,
        "${"Delete task @id?".tlParams({'id': '#$id'})}\n"
            "${"Only the task record is removed; downloaded files stay.".tl}",
      );
      if (!ok) return;
    }    setState(() => _busy.add(id));
    final result = await NasClient.instance.jobAction(id, action);
    if (!mounted) return;
    setState(() => _busy.remove(id));
    if (result == null) {
      context.showMessage(message: "Operation failed".tl);
      return;
    }
    if (result.startsWith('!')) {
      context.showMessage(message: result.substring(1));
      return;
    }
    // 操作成功后立刻重拉：SSE 的 jobs 帧最多 2 秒才到，用户点了要有反馈
    _load(silent: true);
  }

  Future<void> _onMenuAction(String action) async {
    switch (action) {
      case 'pause-all':
        if (!await _confirm("Pause all".tl, "Pause all queued tasks?".tl)) {
          return;
        }
        await _bulk('pause-all');
        break;
      case 'resume-all':
        await _bulk('resume-all');
        break;
      case 'retry-failed':
        await _bulk('retry-failed');
        break;
      case 'delete-finished':
        if (!await _confirm(
          "Clear finished tasks".tl,
          "${"Remove all completed / failed / cancelled task records?".tl}\n"
              "${"Downloaded files are kept.".tl}",
        )) {
          return;
        }
        await _bulk('delete-finished');
        break;
      case 'backfill':
        await _backfillFavorites();
        break;
    }
  }

  Future<void> _bulk(String action) async {
    final n = await NasClient.instance.bulkJobs(action);
    if (!mounted) return;
    if (n == null) {
      context.showMessage(message: "Operation failed".tl);
      return;
    }
    context.showMessage(
      message: "Affected @n tasks".tlParams({'n': '$n'}),
    );
    _load(silent: true);
  }

  /// 补录"收藏了但没缓存 / 只缓存了一部分"的漫画。
  ///
  /// 两步：先 dryRun 拿计划摊给用户看，确认后再真正入队。
  /// 不能直接一键入队——收藏可能几百本，而真正需要下的也许只有个位数，
  /// 盲点一下就往队列里塞几百条。
  ///
  /// 预览里会显示 `Incremental`（只缺新章节的本数）：这个数不为 0
  /// 说明有书以前以为下全了、其实源站更新过。
  Future<void> _backfillFavorites() async {
    final plan = await NasClient.instance.queueFavorites(dryRun: true);
    if (!mounted) return;
    if (plan == null) {
      context.showMessage(message: "Operation failed".tl);
      return;
    }
    if (plan.candidates == 0) {
      context.showMessage(
        message: "No favorites need backfilling".tl,
      );
      return;
    }

    final bySource = plan.bySource.entries
        .map((e) => '${e.key}:${e.value}')
        .join('  ');
    final preview = plan.items
        .take(8)
        .map((it) => '· [${it.source}] ${it.title.isEmpty ? it.comicId : it.title}'
            '${it.missing > 0 ? " (+${it.missing})" : ""}')
        .join('\n');
    final more =
        plan.candidates > 8 ? '\n… ${"and @n more".tlParams({'n': '${plan.candidates - 8}'})}' : '';

    final ok = await _confirm(
      "Backfill missing favorites".tl,
      "${"Will queue @n comics".tlParams({'n': '${plan.candidates}'})}\n"
          "${"By source: @s".tlParams({'s': bySource})}\n"
          "${"Skipped: @c cached, @q queued".tlParams({'c': '${plan.alreadyCached}', 'q': '${plan.alreadyQueued}'})}"
          "${plan.incremental > 0 ? "\n${"Of which @n only lack new chapters".tlParams({'n': '${plan.incremental}'})}" : ""}"
          "\n\n$preview$more",
    );
    if (!ok) return;

    final done = await NasClient.instance.queueFavorites(dryRun: false);
    if (!mounted) return;
    if (done == null) {
      context.showMessage(message: "Operation failed".tl);
      return;
    }
    context.showMessage(
      message: "${"Queued @n comics".tlParams({'n': '${done.queued}'})}"
          "${done.failed > 0 ? " · ${"failed @n".tlParams({'n': '${done.failed}'})}" : ""}",
    );
    Log.info('NAS', 'backfill queued ${done.queued}');
    _load(silent: true);
  }

  /// 二次确认。返回 true 表示用户点了确认。
  ///
  /// showConfirmDialog 是回调式（不返回 bool），这里用 Completer 桥接；
  /// 对话框被点遮罩关掉时也要 resolve，否则 await 会永远挂着。
  Future<bool> _confirm(String title, String content) async {
    final completer = Completer<bool>();
    showConfirmDialog(
      context: context,
      title: title,
      content: content,
      onConfirm: () {
        if (!completer.isCompleted) completer.complete(true);
      },
    ).then((_) {
      if (!completer.isCompleted) completer.complete(false);
    });
    return completer.future;
  }
}
