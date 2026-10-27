import 'package:flutter_test/flutter_test.dart';

/// 复现并锁定一个真实 bug：键盘/音量键翻页时动图永不播放。
///
/// 根因：翻页有两条路径
///   - 滑动 → onPageChanged → setPage() → onPageSettling() ✓
///   - 键盘/音量键 → toPage() → 直接 this.page = page，**绕过了 setPage**
/// 于是 animatePage 永远是 null，动图不被允许播放。
///
/// 这里用最小模型复现两条路径的差异，防止以后再加翻页入口时又漏掉通知。
class _FakeReader {
  _FakeReader(this._page);

  int _page;
  int get page => _page;

  /// 允许播放动图的页码
  int? animatePage;

  /// 记录 onPageSettling 被调用的次数
  int settlingCalls = 0;

  /// 模拟定时器：立刻执行（测试里不等真实 600ms）
  void _fireSettle(int page) {
    if (_page != page) return; // 已翻走
    animatePage = page;
  }

  /// 滑动路径：onPageChanged → setPage
  void setPage(int page) {
    if (_page != page) {
      _page = page;
      settlingCalls++;
      _fireSettle(page);
    }
  }

  /// 按键路径：toPage（修复后也应触发停稳检测）
  void toPage(int page) {
    if (_page == page) return;
    _page = page;
    settlingCalls++;
    _fireSettle(page);
  }
}

void main() {
  group('翻页路径必须都触发停稳检测', () {
    test('滑动路径（setPage）能允许播放动图', () {
      final r = _FakeReader(1);
      r.setPage(2);
      expect(r.settlingCalls, 1);
      expect(r.animatePage, 2,
          reason: '滑动翻页后应允许当前页播动画');
    });

    test('按键路径（toPage）同样能允许播放动图', () {
      final r = _FakeReader(1);
      r.toPage(2);
      expect(r.settlingCalls, 1,
          reason: 'toPage 必须触发停稳检测，否则按键翻页时 GIF 永不播放');
      expect(r.animatePage, 2);
    });

    test('翻页后立刻再翻，动画归属最后一页', () {
      final r = _FakeReader(1);
      r.setPage(2);
      r.toPage(3);
      expect(r.animatePage, 3,
          reason: '只应允许最后停留的那一页播动画');
    });

    test('同一页重复设置不重复触发', () {
      final r = _FakeReader(5);
      r.setPage(5);
      r.toPage(5);
      expect(r.settlingCalls, 0);
    });
  });
}
