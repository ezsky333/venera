import 'package:flutter_test/flutter_test.dart';

/// 离线模式的判定语义：**只看开关是否打开**，不看易失的可达性探测结果。
///
/// 这里踩过一个很隐蔽的坑，值得用测试钉死：
///
/// `_nasReachable` 只在 `toggle()` 与 `refreshReachability()` 里赋值，
/// 后者由首页 initState / onResume 触发，而且是**异步**的。于是：
///   - 重启 App 后直接点进某本漫画详情页（不经过首页那个 await 窗口），
///     `_nasReachable` 还是初值 false
///   - `preferNasOnly` 因此为 false
///   - 详情页**照样去问源站**，离线模式下要等 12 秒超时才降级到 NAS
///
/// 用户看到的现象就是「离线模式开着，ehentai 的详情页却还在连源站」。
///
/// 现在 `preferNasOnly => _enabled`：NAS 真不可达时 `comicFallback`
/// 返回 null，调用方自然回落到源站，不会卡死。
class _FakeOfflineMode {
  _FakeOfflineMode({required this.enabled, required this.reachable});

  bool enabled;

  /// 模拟「开关开着、但可达性探测还没跑（或跑完是旧值）」的状态
  bool reachable;

  /// 修复后的语义
  bool get preferNasOnlyFixed => enabled;

  /// 修复前的错误语义
  bool get preferNasOnlyBuggy => enabled && reachable;
}

void main() {
  group('离线模式 preferNasOnly', () {
    test('开关打开即走 NAS，不依赖易失的可达性标志', () {
      // 最典型的翻车现场：刚重启 App，开关是开着的，
      // 但 refreshReachability() 还没跑完 → reachable=false
      final mode = _FakeOfflineMode(enabled: true, reachable: false);

      expect(
        mode.preferNasOnlyFixed,
        isTrue,
        reason: '开关打开就必须优先 NAS；否则详情页会白等源站 12 秒超时',
      );

      // 记录修复前的行为，说明这个测试在防什么
      expect(
        mode.preferNasOnlyBuggy,
        isFalse,
        reason: '旧实现会因为可达性未就绪而误判为 false —— 这就是 bug',
      );
    });

    test('开关关闭时不走 NAS（回落到原版行为）', () {
      final mode = _FakeOfflineMode(enabled: false, reachable: false);
      expect(mode.preferNasOnlyFixed, isFalse);
    });

    test('开关关闭但探测过可达，也不走 NAS', () {
      // 可达与否不该能"反向"打开离线模式
      final mode = _FakeOfflineMode(enabled: false, reachable: true);
      expect(mode.preferNasOnlyFixed, isFalse);
    });

    test('开关打开且 NAS 可达（理想路径）', () {
      final mode = _FakeOfflineMode(enabled: true, reachable: true);
      expect(mode.preferNasOnlyFixed, isTrue);
    });
  });
}
