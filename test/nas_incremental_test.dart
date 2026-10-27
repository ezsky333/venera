import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/nas_client.dart';

void main() {
  group('NasQueuePlan.fromJson', () {
    test('解析服务端补录计划（含 incremental 与明细）', () {
      final plan = NasQueuePlan.fromJson({
        'candidates': 3,
        'queued': 0,
        'alreadyQueued': 1,
        'alreadyCached': 440,
        'incremental': 2,
        'failed': 0,
        'bySource': {'jm': 2, 'picacg': 1},
        'items': [
          {'source': 'jm', 'comicId': '123', 'title': '书A', 'missing': 2},
          {'source': 'jm', 'comicId': '456', 'title': '书B', 'missing': 5},
        ],
        'dryRun': true,
      });

      expect(plan.candidates, 3);
      expect(plan.queued, 0);
      expect(plan.alreadyQueued, 1);
      expect(plan.alreadyCached, 440);
      // 这个数字是本次修复的核心：它表示"以为下全了、其实缺新章节"的本数
      expect(plan.incremental, 2);
      expect(plan.bySource['jm'], 2);
      expect(plan.bySource['picacg'], 1);
      expect(plan.items.length, 2);
      expect(plan.items.first.missing, 2);
      expect(plan.dryRun, isTrue);
    });

    test('字段缺失时不抛异常（老服务端兼容）', () {
      final plan = NasQueuePlan.fromJson({});
      expect(plan.candidates, 0);
      expect(plan.incremental, 0);
      expect(plan.bySource, isEmpty);
      expect(plan.items, isEmpty);
      expect(plan.dryRun, isFalse);
    });
  });

  group('NasDownloadRequest 语义', () {
    test('queued=true 表示排上了任务', () {
      const r = NasDownloadRequest(
        queued: true,
        jobId: 42,
        total: 12,
        cached: 10,
        missing: 2,
      );
      expect(r.jobId, 42);
      expect(r.isUpToDate, isFalse);
      expect(r.isAlreadyQueued, isFalse);
      // 只补 2 章时必须能算出"这不是整本重下"
      expect(r.missing, lessThan(r.total));
    });

    test('up-to-date 不是失败', () {
      const r = NasDownloadRequest(
        queued: false,
        reason: 'up-to-date',
        total: 10,
        cached: 10,
      );
      expect(r.isUpToDate, isTrue);
      expect(r.jobId, isNull);
    });

    test('already-queued 单独区分（否则会被当成已下载完成）', () {
      const r = NasDownloadRequest(queued: false, reason: 'already-queued');
      expect(r.isAlreadyQueued, isTrue);
      expect(r.isUpToDate, isFalse);
    });
  });

  group('NasJobPage', () {
    test('total 独立于本页数量', () {
      const page = NasJobPage(jobs: [], total: 1234);
      expect(page.isEmpty, isTrue);
      expect(page.total, 1234);
    });
  });
}
