import 'dart:async' show Future, StreamController, scheduleMicrotask;
import 'dart:convert';
import 'dart:math';
import 'dart:ui' as ui show Codec;
import 'dart:ui';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:venera/foundation/cache_manager.dart';
import 'package:venera/foundation/log.dart';

abstract class BaseImageProvider<T extends BaseImageProvider<T>>
    extends ImageProvider<T> {
  const BaseImageProvider();

  static const int maxImagePixel = 2560 * 1440;

  static TargetImageSize _getTargetSize(int width, int height) {
    // ignore invalid size
    if (width <= 0 || height <= 0) {
      return TargetImageSize(width: width, height: height);
    }
    // ignore too wide or too tall image
    final imageRatio = width / height;
    if (imageRatio > 2 || imageRatio < 0.5) {
      return TargetImageSize(width: width, height: height);
    }
    // resize if too large
    if (width * height > maxImagePixel) {
      final ratio = sqrt(maxImagePixel / (width * height));
      return TargetImageSize(width: (width * ratio).round(), height: (height * ratio).round());
    }
    return TargetImageSize(width: width, height: height);
  }

  /// 单张动图允许解码占用的最大内存（字节）。
  ///
  /// 背景：实测《[DMM.com] 神殺しのアリア》里有 232 个动图 GIF。
  /// Flutter 的 MultiFrameImageStreamCompleter 会把**所有帧**解进内存
  /// 并持续播放动画，阅读器默认还预加载 3 张，峰值可达 600MB+，
  /// 于是整页卡顿、滑动掉帧（用户报告"滑动都会卡顿，尤其它还有 gif"）。
  ///
  /// 注意这跟 NAS 无关——源站在线读同一本一样卡。之前误判成
  /// "NAS 图片没降采样"，实测 getTargetSize 反而更慢（65ms→94ms，
  /// 因为要先全解码再缩放），那个方向是错的，已回退。
  ///
  /// 阈值用**内存字节**而不是像素数：实测同一个目录里既有
  /// 1080x498 x 108 帧（232MB），也有 380x214 x 301 帧（98MB）。
  /// 后者像素数不高（24M）但帧数多，按像素判断会漏掉。
  /// 统一按 帧数 × 单帧 RGBA 字节 来算才准确。
  ///
  /// 24MB 的取值理由：普通静态图单帧通常 < 8MB，永远碰不到；
  /// 而 3 张预加载加起来上限 72MB，在手机上属于安全范围。
  static const int maxAnimatedBytes = 24 * 1000 * 1000;

  /// 动图解码后超过 [maxAnimatedBytes] 时只保留首帧。
  ///
  /// 刻意的取舍：宁可看到一张静态图，也不要整个阅读器卡死。
  /// 用户若确实想看动画，可长按保存后用系统图库打开。
  static Future<ui.Codec> limitAnimatedFrames(ui.Codec codec) async {
    if (codec.frameCount <= 1) return codec;

    // 探一帧拿到尺寸（这一帧不保留，后面由包装后的 codec 重新取）
    final probe = await codec.getNextFrame();
    final w = probe.image.width;
    final h = probe.image.height;
    final frameCount = codec.frameCount;
    probe.image.dispose();

    // RGBA 每像素 4 字节
    final bytes = w * h * 4 * frameCount;
    if (bytes <= maxAnimatedBytes) return codec;

    Log.info(
      'Image Loading',
      'animated image too heavy: ${w}x$h x $frameCount frames = '
          '${(bytes / 1e6).toStringAsFixed(0)}MB decoded, first frame only',
    );
    return _FirstFrameOnlyCodec(codec);
  }

  @override
  ImageStreamCompleter loadImage(T key, ImageDecoderCallback decode) {
    final chunkEvents = StreamController<ImageChunkEvent>();
    return MultiFrameImageStreamCompleter(
      codec: _loadBufferAsync(key, chunkEvents, decode),
      chunkEvents: chunkEvents.stream,
      scale: 1.0,
      informationCollector: () sync* {
        yield DiagnosticsProperty<ImageProvider>(
          'Image provider: $this \n Image key: $key',
          this,
          style: DiagnosticsTreeStyle.errorProperty,
        );
      },
    );
  }

  Future<ui.Codec> _loadBufferAsync(
    T key,
    StreamController<ImageChunkEvent> chunkEvents,
    ImageDecoderCallback decode,
  ) async {
    try {
      int retryTime = 1;

      bool stop = false;

      chunkEvents.onCancel = () {
        stop = true;
      };

      Uint8List? data;

      while (data == null && !stop) {
        try {
          data = await load(chunkEvents, () {
            if (stop) {
              throw const _ImageLoadingStopException();
            }
          });
        } on _ImageLoadingStopException {
          rethrow;
        } catch (e) {
          if (e.toString().contains("Invalid Status Code: 404")) {
            rethrow;
          }
          if (e.toString().contains("Invalid Status Code: 403")) {
            rethrow;
          }
          if (e.toString().contains("handshake")) {
            if (retryTime < 5) {
              retryTime = 5;
            }
          }
          retryTime <<= 1;
          if (retryTime > (1 << 3) || stop) {
            rethrow;
          }
          await Future.delayed(Duration(seconds: retryTime));
        }
      }

      if (stop) {
        throw const _ImageLoadingStopException();
      }

      if (data!.isEmpty) {
        throw Exception("Empty image data");
      }

      try {
        final buffer = await ImmutableBuffer.fromUint8List(data);
        final codec = await decode(
          buffer,
          getTargetSize: enableResize ? _getTargetSize : null,
        );
        // 动图护栏：超大动图只保留首帧，避免一次性解出几百 MB。
        // 放在这里而不是 loadImage，是因为只有 decode 之后才知道
        // 帧数和单帧尺寸。
        return await limitAnimatedFrames(codec);
      } catch (e) {
        await CacheManager().delete(this.key);
        if (data.length < 2 * 1024) {
          // data is too short, it's likely that the data is text, not image
          try {
            var text =
                const Utf8Codec(allowMalformed: false).decoder.convert(data);
            throw Exception("Expected image data, but got text: $text");
          } catch (e) {
            // ignore
          }
        }
        rethrow;
      }
    } on _ImageLoadingStopException {
      rethrow;
    } catch (e, s) {
      scheduleMicrotask(() {
        PaintingBinding.instance.imageCache.evict(key);
      });
      Log.error("Image Loading", e, s);
      rethrow;
    } finally {
      chunkEvents.close();
    }
  }

  Future<Uint8List> load(
    StreamController<ImageChunkEvent> chunkEvents,
    void Function() checkStop,
  );

  String get key;

  @override
  bool operator ==(Object other) {
    return other is BaseImageProvider<T> && key == other.key;
  }

  @override
  int get hashCode => key.hashCode;

  @override
  String toString() {
    return "$runtimeType($key)";
  }

  bool get enableResize => false;
}

typedef FileDecoderCallback = Future<ui.Codec> Function(Uint8List);

/// 只暴露首帧的 [ui.Codec] 包装。
///
/// 用于把超大动图降级成静态图：frameCount 报 1，
/// 后续 getNextFrame 每次都返回同一帧。
/// MultiFrameImageStreamCompleter 看到 frameCount<=1 就不会再排动画定时器，
/// 于是既省内存也不再持续占用 CPU。
///
/// 实现要点：首帧只解一次并缓存。原 codec 在拿到首帧后立刻 dispose，
/// 否则它内部仍然持有多帧缓冲。
class _FirstFrameOnlyCodec implements ui.Codec {
  _FirstFrameOnlyCodec(this._inner);

  final ui.Codec _inner;

  // 注意：这里用无前缀的 FrameInfo。
  // 文件顶部 `import 'dart:ui' as ui show Codec;` 只导出了 Codec，
  // 所以 ui.FrameInfo 不可见；但下一行的 `import 'dart:ui';`
  // 是无前缀全量导入，FrameInfo 直接可用。
  FrameInfo? _frame;

  @override
  int get frameCount => 1;

  @override
  int get repetitionCount => 0;

  @override
  Future<FrameInfo> getNextFrame() async {
    final cached = _frame;
    if (cached != null) return cached;

    final f = await _inner.getNextFrame();
    // 把图像留在这里复用，避免每次 build 都重新解码一帧
    _frame = f;
    // 原 codec 只取一帧即可释放，避免它继续持有后续帧的解码状态
    _inner.dispose();
    return f;
  }

  @override
  void dispose() {
    _frame?.image.dispose();
    _frame = null;
  }
}

class _ImageLoadingStopException implements Exception {
  const _ImageLoadingStopException();
}
