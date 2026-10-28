import 'package:flutter_test/flutter_test.dart';

import 'package:venera/utils/app_update.dart';

void main() {
  group('isNewerVersion', () {
    test('补丁位更大', () {
      expect(isNewerVersion('1.6.6', '1.6.3'), isTrue);
      expect(isNewerVersion('1.6.3', '1.6.6'), isFalse);
    });

    test('相同版本不算更新', () {
      // 这条是关键：发布 tag 与 App 报的版本号对齐之后，
      // 装到最新版的人不该再被提示"有新版本"。
      expect(isNewerVersion('1.6.6', '1.6.6'), isFalse);
    });

    test('十进制按数字比，不按字符串比', () {
      // 字符串比较会把 1.10 判成小于 1.9
      expect(isNewerVersion('1.10.0', '1.9.0'), isTrue);
      expect(isNewerVersion('1.9.0', '1.10.0'), isFalse);
    });

    test('缺段按 0 处理', () {
      expect(isNewerVersion('1.7', '1.6.9'), isTrue);
      expect(isNewerVersion('1.7', '1.7.0'), isFalse);
      expect(isNewerVersion('1.7.0', '1.7'), isFalse);
    });

    test('主版本号优先于次版本号', () {
      expect(isNewerVersion('2.0.0', '1.99.99'), isTrue);
      expect(isNewerVersion('1.99.99', '2.0.0'), isFalse);
    });
  });
}
