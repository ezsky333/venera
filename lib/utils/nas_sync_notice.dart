import 'package:flutter/material.dart';

import 'package:venera/components/components.dart';
import 'package:venera/foundation/app.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/utils/nas_sync.dart';
import 'package:venera/utils/translations.dart';

/// "同步中"提示的最短显示时长。
const minProgressVisible = Duration(milliseconds: 1200);

/// App 启动时同步一次，并用底部提示把结果告诉用户。
///
/// 三条文案（与需求一致）：
///   - 当前已是最新版本               云端没有新数据
///   - 检测到云端数据有更新，同步中    刚发现云端有新数据的瞬间
///   - 同步完成                      新数据已经落地
///
/// 服务端不可达 / NAS 未启用 / 已有同步在跑时**不提示**：这几种情况在
/// 离开局域网等场景下每次冷启动都会出现，每次都弹一条只会变成噪音
/// （错误已经记进日志，设置页也能看到 lastError）。
Future<void> announceNasStartupSync() async {
  if (!NasClient.instance.isEnabled) return;

  // 启动同步必须留痕。曾经这里完全没有日志，于是"新设备连了 NAS
  // 但历史一条都没同步"只能靠猜——无法区分"没跑"、"probe 失败"
  // 还是"拉到了 0 条"。
  Log.info('NAS Sync', 'startup sync begin '
      '(baseUrl=${NasClient.instance.config.baseUrl}, '
      'cursor=${NasClient.instance.config.cursor})');

  // 提示挂在 OverlayWidget 上，它要等第一帧之后才存在。
  // 本地 NAS 响应很快，同步经常比首帧还早结束——不等的话提示会被吞掉。
  //
  // 注意：等不到就**只跳过提示**，同步本身照常执行。同步是功能，
  // 提示是附带的，不能因为界面没就绪就不同步了。
  final canToast = await _waitForToastHost();

  VoidCallback? dismissProgress;
  DateTime? progressShownAt;

  void showProgress() {
    // 收起旧的那条再弹，避免两条提示叠在同一位置
    dismissProgress?.call();
    dismissProgress =
        _toast('Cloud data updated, syncing'.tl, seconds: 30);
    progressShownAt = DateTime.now();
  }

  final result = await NasSync()
      .start(onRemoteChanges: canToast ? showProgress : null);

  Log.info('NAS Sync', 'startup sync done: $result (canToast=$canToast)');

  if (!canToast) return;

  // 局域网 NAS 上拉取往往只要几百毫秒，"同步中"会一闪而过、
  // 根本看不清。这里保证它至少显示 [minProgressVisible]，
  // 让"检测到更新 → 同步完成"两步都读得出来。
  if (progressShownAt != null) {
    final shown = DateTime.now().difference(progressShownAt!);
    if (shown < minProgressVisible) {
      await Future.delayed(minProgressVisible - shown);
    }
  }

  // "同步中"必须显式收起：OverlayWidget 允许多条提示共存，
  // 不收起会和新提示完全重叠。
  dismissProgress?.call();
  dismissProgress = null;

  switch (result.outcome) {
    case NasSyncOutcome.updated:
      _toast('Sync completed'.tl);
      break;
    case NasSyncOutcome.upToDate:
      _toast('Current version is up to date'.tl);
      break;
    case NasSyncOutcome.failed:
    case NasSyncOutcome.disabled:
    case NasSyncOutcome.busy:
      break;
  }
}

/// 等 OverlayWidget 挂载完成（也就是首帧渲染完毕）。
///
/// 返回 false 表示等到超时也没准备好，调用方应当只跳过提示。
Future<bool> _waitForToastHost({
  Duration timeout = const Duration(seconds: 20),
}) async {
  final deadline = DateTime.now().add(timeout);
  while (DateTime.now().isBefore(deadline)) {
    final ctx = App.rootNavigatorKey.currentContext;
    if (ctx != null &&
        ctx.mounted &&
        ctx.findAncestorStateOfType<OverlayWidgetState>() != null) {
      return true;
    }
    await Future.delayed(const Duration(milliseconds: 100));
  }
  Log.error('NAS Sync', 'toast host not ready, skip startup notice');
  return false;
}

VoidCallback _toast(String message, {int seconds = 2}) {
  return showToast(
    message: message,
    context: App.rootContext,
    seconds: seconds,
  );
}
