import 'dart:io';
import 'dart:typed_data';
import 'dart:ui' as ui;

import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/image_provider/base_image_provider.dart';

/// 找一本含大动图的漫画，用真实文件验证限帧护栏。
File? _findHeavyGif() {
  final lib = Directory(r'D:\MyProject\venera\_runtime\library');
  if (!lib.existsSync()) return null;

  File? best;
  var bestFrames = 0;
  for (final f in lib.listSync(recursive: true)) {
    if (f is! File) continue;
    if (!f.path.toLowerCase().endsWith('.gif')) continue;
    // 只看 >3MB 的，避免遍历全部
    if (f.lengthSync() < 3 * 1000 * 1000) continue;
    if (best == null || f.lengthSync() > best.lengthSync()) best = f;
    bestFrames++;
  }
  return best;
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  test('超大动图被降级为首帧', () async {
    final file = _findHeavyGif();
    if (file == null) {
      markTestSkipped('no heavy gif found');
      return;
    }

    final bytes = Uint8List.fromList(file.readAsBytesSync());
    print('gif: ${file.path.split(Platform.pathSeparator).last} '
        '${(bytes.length / 1e6).toStringAsFixed(2)} MB');

    final buffer = await ui.ImmutableBuffer.fromUint8List(bytes);
    final codec = await ui.instantiateImageCodecWithSize(buffer);
    print('frameCount = ${codec.frameCount}');

    final limited = await BaseImageProvider.limitAnimatedFrames(codec);
    print('after limit: frameCount = ${limited.frameCount}');

    final frame = await limited.getNextFrame();
    print('first frame: ${frame.image.width}x${frame.image.height}');
    expect(frame.image.width, greaterThan(0));

    // 关键断言：超大动图必须被降成 1 帧
    expect(limited.frameCount, 1);

    // 再取一次应返回同一帧（不重新解码）
    final frame2 = await limited.getNextFrame();
    expect(identical(frame, frame2), isTrue,
        reason: 'second getNextFrame must reuse the cached frame');

    limited.dispose();
  });

  test('静态图不受影响', () async {
    final lib = Directory(r'D:\MyProject\venera\_runtime\library');
    if (!lib.existsSync()) {
      markTestSkipped('library not found');
      return;
    }
    File? jpg;
    for (final f in lib.listSync(recursive: true)) {
      if (f is! File) continue;
      final p = f.path.toLowerCase();
      if (p.endsWith('.jpg') || p.endsWith('.webp')) {
        if (f.lengthSync() > 100 * 1000) {
          jpg = f;
          break;
        }
      }
    }
    if (jpg == null) {
      markTestSkipped('no static image found');
      return;
    }

    final bytes = Uint8List.fromList(jpg.readAsBytesSync());
    final buffer = await ui.ImmutableBuffer.fromUint8List(bytes);
    final codec = await ui.instantiateImageCodecWithSize(buffer);
    final before = codec.frameCount;
    final limited = await BaseImageProvider.limitAnimatedFrames(codec);
    // 静态图原样返回
    expect(limited.frameCount, before);
    expect(identical(limited, codec), isTrue);
  });
}
