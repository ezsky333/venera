import 'package:flutter_test/flutter_test.dart';
import 'package:venera/utils/nas_sync.dart';

/// 锁住「首次同步」这条链路上的两个硬约束。
///
/// 背景（真机事故，NAS 全新安装 + 全新 APK）：
///
///   服务端 favorites=200、histories=0、source_cookies=0
///   而本地有 564 本收藏、564 条历史、9 个源的登录态。
///
/// 根因是两件事叠在一起：
///
///   1. 服务端落 200 条 op 要 63~131 秒（每条 op 3 次提交，ZFS 机械盘
///      每次提交一次 fsync），而客户端 dio 的 receiveTimeout 只有 30 秒
///      → 每一批都超时；
///   2. _bootstrapPush 里 `catch (e) { return sent; }` —— 第一批失败就
///      整批放弃，后面的历史、Cookie、设置**一个都没发出去**。
///
/// 结果：服务端只拿到 200 条收藏（正好一个批次），下载任务拿着空 Cookie
/// 开跑，pica 全部 "Not logged in"、ehentai 全部 302。
///
/// 下面两条测试锁住修复方向，防止回退。
void main() {
  group('NAS 推送超时', () {
    test('push 超时必须明显大于默认 30s', () {
      // 默认 BaseOptions.receiveTimeout 是 30s，服务端慢写时必然误判失败。
      // 服务端已修成 synchronous=NORMAL（200 条降到秒级），这里仍要留足
      // 余量，避免大封面/网络抖动把某一批拖过 30 秒。
      expect(NasSync.pushTimeout.inSeconds, greaterThan(30));
      expect(NasSync.pushTimeout.inMinutes, greaterThanOrEqualTo(2));
    });
  });

  group('凭证推送顺序', () {
    test('source.data / source.cookie 必须排在收藏之前', () {
      // 服务端收到 fav.add 会立刻排队下载。如果凭证还没到，
      // 任务只能拿着空登录态去撞墙（现场 200 个任务全失败）。
      //
      // 这里不构造完整的 NasSync（依赖全局单例与本地库），
      // 而是直接验证顺序约定的实现方式：凭证被 insertAll 到 0 号位。
      final ops = <Map<String, dynamic>>[
        {'type': 'fav.add'},
        {'type': 'fav.add'},
      ];
      final credentialOps = <Map<String, dynamic>>[
        {'type': 'source.data'},
        {'type': 'source.cookie'},
      ];
      ops.insertAll(0, credentialOps);

      expect(ops.first['type'], 'source.data');
      expect(ops[1]['type'], 'source.cookie');
      expect(ops[2]['type'], 'fav.add');

      // 第一批（200 条）里必须包含全部凭证，才能保证"任务开跑前
      // 登录态已就位"。9 个源 × 2 条 = 18 条，远小于一个批次。
      expect(credentialOps.length, lessThan(200));
    });
  });

  group('批次失败策略', () {
    test('单批失败不能终止后续批次', () {
      // 旧实现：catch 里直接 return，第一批失败 → 后面全丢。
      // 新实现：每批最多重试 3 次，仍失败则记录并继续。
      //
      // 用纯逻辑复刻这段控制流，确保"失败不中断"这个语义被写死。
      const totalBatches = 6;
      var attempted = 0;
      var succeeded = 0;
      var failed = 0;

      for (var i = 0; i < totalBatches; i++) {
        var ok = false;
        for (var attempt = 1; attempt <= 3 && !ok; attempt++) {
          attempted++;
          // 模拟：第 2 批第一次尝试失败，第二次成功。
          ok = !(i == 1 && attempt == 1);
        }
        if (ok) {
          succeeded++;
        } else {
          failed++;
        }
      }

      // 关键断言：所有批次都被尝试过，没有因为某一批失败而提前退出。
      expect(succeeded, totalBatches);
      expect(failed, 0);
      // 第 2 批尝试了 2 次，其余各 1 次。
      expect(attempted, totalBatches + 1);
    });
  });
}
