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
/// 实时性设计（与 Web 管理面板一致）：
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

  /// jobId → 实时状态（来自 SSE live 帧）。
  ///
  /// 单独存放而不是合并进 _jobs，是为了让 progress 变化时
  /// 只触发进度条重绘，不重建整行 Widget。
  Map<int, NasJobLive> _live = {};

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
        _loading = false;
        _error = null;
        // 清理已不在列表里的实时状态，避免内存增长
        final ids = ev.jobs.map((j) => (j['id'] as num?)?.toInt() ?? 0).toSet();
        _live.removeWhere((k, _) => !ids.contains(k));
      });
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
    final jobs = await NasClient.instance.jobs(
      status: _filter.isEmpty ? null : _filter,
    );
    if (!mounted) return;
    setState(() {
      _jobs = jobs;
      _loading = false;
      _error = null;
    });
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
          IconButton(
            tooltip: "Retry failed NAS tasks".tl,
            icon: const Icon(Icons.refresh),
            onPressed: _retryFailed,
          ),
        ],
      ),
      body: Column(
        children: [
          _buildFilterBar(),
          if (_error != null)
            Expanded(child: NetworkError(message: _error!, retry: _load))
          else if (_loading)
            const Expanded(child: Center(child: CircularProgressIndicator()))
          else if (_jobs.isEmpty)
            Expanded(
              child: Center(child: Text("No NAS downloads".tl)),
            )
          else
            Expanded(
              child: ListView.separated(
                itemCount: _jobs.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (context, i) => _buildJobRow(_jobs[i]),
              ),
            ),
        ],
      ),
    );
  }

  Widget _buildFilterBar() {
    const filters = [
      ['', 'All'],
      ['running', 'Downloading'],
      ['pending', 'Queued'],
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
                  setState(() => _filter = f[0]);
                  // 筛选变了要重新订阅（服务端按 status 过滤 jobs 帧）
                  _connectEvents();
                  _load();
                },
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
      title: Text(title, maxLines: 1, overflow: TextOverflow.ellipsis),
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
      trailing: isActive
          ? IconButton(
              icon: const Icon(Icons.close),
              tooltip: "Cancel".tl,
              onPressed: () async {
                final ok = await NasClient.instance.cancelJob(id);
                if (!context.mounted) return;
                if (ok) {
                  context.showMessage(message: "Cancelled".tl);
                  _load(silent: true);
                }
              },
            )
          : null,
    );
  }

  Future<void> _retryFailed() async {
    final n = await NasClient.instance.retryFailed();
    if (!mounted) return;
    context.showMessage(message: n > 0 ? "Retry failed NAS tasks".tl : "None".tl);
    Log.info('NAS', 'retried $n failed jobs');
    _load();
  }
}
