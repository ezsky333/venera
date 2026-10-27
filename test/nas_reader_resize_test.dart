import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/nas_client.dart';

void main() {
  group('NAS 图片降采样判定', () {
    // 阅读器用 NasClient.isNasUrl 决定是否为 NAS 图片开启降采样解码。
    // 这里固化它的判定语义，避免以后改 URL 拼装方式时悄悄失效
    // （一旦失效就回到"整张 10MB 原图解码"，翻页重新变卡）。
    setUp(() {
      // isNasUrl 依赖内部配置，未配置时应一律 false
    });

    test('未配置 NAS 时任何 URL 都不是 NAS URL', () {
      expect(NasClient.isNasUrl(''), isFalse);
      expect(NasClient.isNasUrl('https://i3.nhentai.net/galleries/1/1.jpg'),
          isFalse);
      expect(NasClient.isNasUrl('http://127.0.0.1:18080/api/v1/media/x/0.jpg'),
          isFalse);
    });
  });

  group('NasDownloadRequest 增量语义', () {
    test('missing < total 表示只补部分章节', () {
      const r = NasDownloadRequest(
        queued: true,
        jobId: 1,
        total: 11,
        cached: 8,
        missing: 3,
      );
      // 详情页据此提示"已加入队列（缺 3 章）"而不是整本重下
      expect(r.missing, 3);
      expect(r.cached + r.missing, r.total);
    });

    test('up-to-date 不应被当成失败', () {
      const r = NasDownloadRequest(queued: false, reason: 'up-to-date');
      expect(r.isUpToDate, isTrue);
      expect(r.isAlreadyQueued, isFalse);
      expect(r.jobId, isNull);
    });
  });
}
