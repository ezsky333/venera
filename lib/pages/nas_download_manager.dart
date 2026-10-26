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

  Timer? _poll;

  @override
  void initState() {
    super.initState();
    _load();
    // 5 秒轮询，只在页面存活期间跑
    _poll = Timer.periodic(const Duration(seconds: 5), (_) {
      if (mounted && !_loading) _load(silent: true);
    });
  }

  @override
  void dispose() {
    _poll?.cancel();
    super.dispose();
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
                  _load();
                },
              ),
            ),
        ],
      ),
    );
  }

  Widget _buildJobRow(Map<String, dynamic> job) {
    final status = job['status']?.toString() ?? '';
    final progress = (job['progress'] as num?)?.toDouble() ?? 0;
    final title = job['title']?.toString().trim().isNotEmpty == true
        ? job['title'].toString()
        : (job['comicId']?.toString() ?? '');
    final source = job['source']?.toString() ?? '';
    final message = job['message']?.toString() ?? '';
    final error = job['error']?.toString() ?? '';
    final id = (job['id'] as num?)?.toInt() ?? 0;

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
      case 'cancelled':
        color = Theme.of(context).colorScheme.outline;
        label = "Cancelled".tl;
        break;
      default:
        color = Colors.orange;
        label = "Queued".tl;
    }

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
          Row(
            children: [
              Text(label, style: TextStyle(color: color, fontSize: 12)),
              const SizedBox(width: 8),
              Text('$source · #$id',
                  style: TextStyle(
                    fontSize: 11,
                    color: Theme.of(context).colorScheme.outline,
                  )),
            ],
          ),
          if (status == 'running' || status == 'pending')
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: LinearProgressIndicator(value: progress),
            ),
          if (message.isNotEmpty && status != 'done')
            Text(message,
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
                style: TextStyle(
                  fontSize: 11,
                  color: Theme.of(context).colorScheme.outline,
                )),
          if (error.isNotEmpty)
            Text(error,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
                style: TextStyle(
                    fontSize: 11,
                    color: Theme.of(context).colorScheme.error)),
        ],
      ),
      isThreeLine: true,
      trailing: status == 'running' || status == 'pending'
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
