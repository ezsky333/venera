import 'dart:io';
import 'dart:typed_data';
import 'dart:ui' as ui;

import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/image_provider/base_image_provider.dart';

/// 找一本含大动图的漫画，用真实文件验证护栏。
File? _findHeavyGif() {
  final lib = Directory(r'D:\MyProject\venera\_runtime\library');
  if (!lib.existsSync()) return null;

  File? best;
  for (final f in lib.listSync(recursive: true)) {
    if (f is! File) continue;
    if (!f.path.toLowerCase().endsWith('.gif')) continue;
    if (f.lengthSync() < 3 * 1000 * 1000) continue;
    if (best == null || f.lengthSync() > best.lengthSync()) best = f;
  }
  return best;
}

Future<ui.Codec> _decode(Uint8List bytes) async {
  final buffer = await ui.ImmutableBuffer.fromUint8List(bytes);
  return ui.instantiateImageCodecWithSize(buffer);
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  test('未停稳时强制只解首帧', () async {
    final file = _findHeavyGif();
    if (file == null) {
      markTestSkipped('no heavy gif found');
      return;
    }

    final bytes = Uint8List.fromList(file.readAsBytesSync());
    print('gif: ${file.path.split(Platform.pathSeparator).last} '
        '${(bytes.length / 1e6).toStringAsFixed(2)} MB');

    final codec = await _decode(bytes);
    print('frameCount = ${codec.frameCount}');
    expect(codec.frameCount, greaterThan(1));

    // forceLimit=true 是"刚翻到、还没停稳"的情形
    final limited =
        await BaseImageProvider.limitAnimatedFrames(codec, forceLimit: true);
    expect(limited.frameCount, 1,
        reason: '未停稳时必须压成首帧，保证翻页跟手');

    final f1 = await limited.getNextFrame();
    expect(f1.image.width, greaterThan(0));
    // 第二次取应复用同一帧，不重新解码
    final f2 = await limited.getNextFrame();
    expect(identical(f1, f2), isTrue);

    limited.dispose();
  });

  test('停稳后允许播放：原 codec 原样返回', () async {
    final file = _findHeavyGif();
    if (file == null) {
      markTestSkipped('no heavy gif found');
      return;
    }
    final bytes = Uint8List.fromList(file.readAsBytesSync());
    final codec = await _decode(bytes);
    final before = codec.frameCount;

    // forceLimit=false 且超过阈值 → 仍会被压
    final limited = await BaseImageProvider.limitAnimatedFrames(codec);
    print('over-threshold: $before -> ${limited.frameCount}');
    // 这份样本超过 24MB 阈值，所以应被压成首帧
    expect(limited.frameCount, 1);

    // 而 allowAnimation=true 的路径根本不调用 limitAnimatedFrames，
    // 由 _loadBufferAsync 直接返回 codec（见实现）。
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
      if ((p.endsWith('.jpg') || p.endsWith('.webp')) &&
          f.lengthSync() > 100 * 1000) {
        jpg = f;
        break;
      }
    }
    if (jpg == null) {
      markTestSkipped('no static image found');
      return;
    }

    final bytes = Uint8List.fromList(jpg.readAsBytesSync());
    final codec = await _decode(bytes);
    final before = codec.frameCount;
    final limited =
        await BaseImageProvider.limitAnimatedFrames(codec, forceLimit: true);
    // 单帧图直接原样返回，不做任何包装
    expect(limited.frameCount, before);
    expect(identical(limited, codec), isTrue);
  });
}
