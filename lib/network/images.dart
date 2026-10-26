import 'dart:async';
import 'dart:typed_data';

import 'package:flutter_qjs/flutter_qjs.dart';
import 'package:venera/foundation/cache_manager.dart';
import 'package:venera/foundation/comic_source/comic_source.dart';
import 'package:venera/foundation/consts.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/foundation/nas_offline_mode.dart';
import 'package:venera/utils/image.dart';

import 'app_dio.dart';

abstract class ImageDownloader {
  static Stream<ImageDownloadProgress> loadThumbnail(
      String url, String? sourceKey,
      [String? cid]) async* {
    final cacheKey = "$url@$sourceKey${cid != null ? '@$cid' : ''}";
    final cache = await CacheManager().findCache(cacheKey);

    if (cache != null) {
      var data = await cache.readAsBytes();
      yield ImageDownloadProgress(
        currentBytes: data.length,
        totalBytes: data.length,
        imageBytes: data,
      );
    }

    // ---- NAS 封面优先 ----
    //
    // 收藏夹、历史、详情页的封面都走 loadThumbnail（CachedImageProvider）。
    // 之前只有 loadComicImage 有 NAS 兜底，导致"服务端明明有封面，
    // 收藏夹和详情页却全空"——列表页加载的是收藏数据里的**源站 URL**，
    // 源站不可达时没有第二来源。
    //
    // 前提：cid 非空（列表页 tile 都会传）；URL 已经是 NAS 地址则不需要。
    // 离线模式打开时抢先取；失败（未缓存/未配置）继续原流程。
    var triedNasCoverPreempt = false;
    if (cid != null &&
        cid.isNotEmpty &&
        sourceKey != null &&
        !NasClient.isNasUrl(url) &&
        NasOfflineMode.instance.preferNasCover) {
      triedNasCoverPreempt = true;
      final nasBytes = await _tryNasCover(sourceKey, cid);
      if (nasBytes != null) {
        await CacheManager().writeCache(cacheKey, nasBytes);
        yield ImageDownloadProgress(
          currentBytes: nasBytes.length,
          totalBytes: nasBytes.length,
          imageBytes: nasBytes,
        );
        return;
      }
    }

    var configs = <String, dynamic>{};
    if (sourceKey != null) {
      var comicSource = ComicSource.find(sourceKey);
      configs = comicSource?.getThumbnailLoadingConfig?.call(url) ?? {};
    }
    configs['headers'] ??= {};
    if (configs['headers']['user-agent'] == null &&
        configs['headers']['User-Agent'] == null) {
      configs['headers']['user-agent'] = webUA;
    }

    if (((configs['url'] as String?) ?? url).startsWith('cover.') &&
        sourceKey != null) {
      var comicSource = ComicSource.find(sourceKey);
      if(comicSource != null) {
        var comicInfo = await comicSource.loadComicInfo!(cid!);
        yield* loadThumbnail(comicInfo.data.cover, sourceKey);
        return;
      }
    }

    var dio = AppDio(BaseOptions(
      headers: Map<String, dynamic>.from(configs['headers']),
      method: configs['method'] ?? 'GET',
      responseType: ResponseType.stream,
    ));

    String requestUrl = configs['url'] ?? url;
    if (requestUrl.startsWith('//')) {
      requestUrl = 'https:$requestUrl';
    }
    var req = await dio.request<ResponseBody>(requestUrl,
        data: configs['data']);
    var stream = req.data?.stream ?? (throw "Error: Empty response body.");
    int? expectedBytes = req.data!.contentLength;
    if (expectedBytes == -1) {
      expectedBytes = null;
    }
    var buffer = <int>[];
    try {
      await for (var data in stream) {
        buffer.addAll(data);
        if (expectedBytes != null) {
          yield ImageDownloadProgress(
            currentBytes: buffer.length,
            totalBytes: expectedBytes,
          );
        }
      }

      if (configs['onResponse'] is JSInvokable) {
        final uint8List = Uint8List.fromList(buffer);
        buffer = (configs['onResponse'] as JSInvokable)([uint8List]);
        (configs['onResponse'] as JSInvokable).free();
      }
    } catch (e) {
      // ---- NAS 封面兜底（无条件层）----
      //
      // 离线模式是"抢先取"；这里是"失败了再问"。源站封面偶尔 404/
      // 超时/被防盗链拦下时，只要 NAS 存过这本书的封面就用它顶上，
      // 不需要用户先打开离线模式开关。
      // 抢先层已经取过（triedNasCoverPreempt）时不再重复请求。
      if (cid != null &&
          cid.isNotEmpty &&
          sourceKey != null &&
          !NasClient.isNasUrl(url) &&
          !triedNasCoverPreempt &&
          NasClient.instance.isEnabled) {
        final nasBytes = await _tryNasCover(sourceKey, cid);
        if (nasBytes != null) {
          await CacheManager().writeCache(cacheKey, nasBytes);
          yield ImageDownloadProgress(
            currentBytes: nasBytes.length,
            totalBytes: nasBytes.length,
            imageBytes: nasBytes,
          );
          return;
        }
      }
      rethrow;
    }

    await CacheManager().writeCache(cacheKey, buffer);
    yield ImageDownloadProgress(
      currentBytes: buffer.length,
      totalBytes: buffer.length,
      imageBytes: Uint8List.fromList(buffer),
    );
  }

  static final _loadingImages = <String, _StreamWrapper<ImageDownloadProgress>>{};

  /// Cancel all loading images.
  static void cancelAllLoadingImages() {
    for (var wrapper in _loadingImages.values) {
      wrapper.cancel();
    }
    _loadingImages.clear();
  }

  /// Load a comic image from the network or cache.
  /// The function will prevent multiple requests for the same image.
  static Stream<ImageDownloadProgress> loadComicImage(
      String imageKey, String? sourceKey, String cid, String eid) {
    final cacheKey = "$imageKey@$sourceKey@$cid@$eid";
    if (_loadingImages.containsKey(cacheKey)) {
      return _loadingImages[cacheKey]!.stream;
    }
    final stream = _StreamWrapper<ImageDownloadProgress>(
      _loadComicImage(imageKey, sourceKey, cid, eid),
      (wrapper) {
        _loadingImages.remove(cacheKey);
      },
    );
    _loadingImages[cacheKey] = stream;
    return stream.stream;
  }

  static Stream<ImageDownloadProgress> loadComicImageUnwrapped(
      String imageKey, String? sourceKey, String cid, String eid) {
    return _loadComicImage(imageKey, sourceKey, cid, eid);
  }

  static Stream<ImageDownloadProgress> _loadComicImage(
      String imageKey, String? sourceKey, String cid, String eid) async* {
    final cacheKey = "$imageKey@$sourceKey@$cid@$eid";
    final cache = await CacheManager().findCache(cacheKey);

    if (cache != null) {
      var data = await cache.readAsBytes();
      yield ImageDownloadProgress(
        currentBytes: data.length,
        totalBytes: data.length,
        imageBytes: data,
      );
    }

    Future<Map<String, dynamic>?> Function()? onLoadFailed;

    // ---- 离线 NAS 模式：封面直接吃 NAS ----
    //
    // 开关打开时源站基本不可用，等 5 次重试全失败再降级是纯粹的浪费：
    // 列表页会先空一片、再逐个补图。这里直接抢先取 NAS 那份。
    //
    // 只处理封面（eid 为空）：内页由阅读器自己的 NAS 优先逻辑负责，
    // 在这里插一层会让两套机制打架。
    if (eid.isEmpty &&
        sourceKey != null &&
        !NasClient.isNasUrl(imageKey) &&
        NasOfflineMode.instance.preferNasCover) {
      final nasBytes = await _tryNasCover(sourceKey, cid);
      if (nasBytes != null) {
        await CacheManager().writeCache(cacheKey, nasBytes);
        yield ImageDownloadProgress(
          currentBytes: nasBytes.length,
          totalBytes: nasBytes.length,
          imageBytes: nasBytes,
        );
        return;
      }
      // NAS 也没这本封面 → 落到下面走原流程（也许源站其实是通的）
    }

    var configs = <String, dynamic>{};
    // NAS 返回的图片 URL 已经是可直取的绝对地址（自带 token），
    // 不能再交给源脚本的 getImageLoadingConfig 处理——那些函数是为
    // 源站图片设计的，会自行拼域名/加 Referer，把 NAS 的 ?token= 丢掉，
    // 于是服务端返回 401。
    if (sourceKey != null && !NasClient.isNasUrl(imageKey)) {
      var comicSource = ComicSource.find(sourceKey);
      configs = (await comicSource!.getImageLoadingConfig
              ?.call(imageKey, cid, eid)) ??
          {};
    }
    var retryLimit = 5;
    // 每次加载最多尝试一次 NAS 封面，避免失败时反复请求
    var triedNasFallback = false;
    while (true) {
      try {
        configs['headers'] ??= {
          'user-agent': webUA,
        };

        if (configs['onLoadFailed'] is JSInvokable) {
          onLoadFailed = () async {
            dynamic result = (configs['onLoadFailed'] as JSInvokable)([]);
            if (result is Future) {
              result = await result;
            }
            if (result is! Map<String, dynamic>) return null;
            return result;
          };
        }

        var dio = AppDio(BaseOptions(
          headers: configs['headers'],
          method: configs['method'] ?? 'GET',
          responseType: ResponseType.stream,
        ));

        var req = await dio.request<ResponseBody>(configs['url'] ?? imageKey,
            data: configs['data']);
        var stream = req.data?.stream ?? (throw "Error: Empty response body.");
        int? expectedBytes = req.data!.contentLength;
        if (expectedBytes == -1) {
          expectedBytes = null;
        }
        var buffer = <int>[];
        await for (var data in stream) {
          buffer.addAll(data);
          yield ImageDownloadProgress(
            currentBytes: buffer.length,
            totalBytes: expectedBytes,
          );
        }

        if (configs['onResponse'] is JSInvokable) {
          dynamic result = (configs['onResponse'] as JSInvokable)([Uint8List.fromList(buffer)]);
          if (result is Future) {
            result = await result;
          }
          if (result is List<int>) {
            buffer = result;
          } else {
            throw "Error: Invalid onResponse result.";
          }
          (configs['onResponse'] as JSInvokable).free();
        }

        Uint8List data;
        if (buffer is Uint8List) {
          data = buffer;
        } else {
          data = Uint8List.fromList(buffer);
          buffer.clear();
        }

        if (configs['modifyImage'] != null) {
          var newData = await modifyImageWithScript(
            data,
            configs['modifyImage'],
          );
          data = newData;
        }

        await CacheManager().writeCache(cacheKey, data);
        yield ImageDownloadProgress(
          currentBytes: data.length,
          totalBytes: data.length,
          imageBytes: data,
        );
        return;
      } catch (e) {
        // ---- 降级到 NAS ----
        //
        // 源站图片拉不到（超时/被墙/该图已被删）时，只要 NAS 上存过这本
        // 漫画的封面，就用 NAS 那份顶上。
        //
        // 触发点刻意放在"重试也失败"之后：正常网络下用户看到的是源站
        // 原图（分辨率通常更高），只有真的拿不到才换成 NAS 缓存。
        //
        // 只对**封面**生效（eid 为空）：阅读器内页已经有独立的 NAS 优先
        // 逻辑，在这里再插一层会让两套机制互相干扰。
        if (!triedNasFallback &&
            eid.isEmpty &&
            sourceKey != null &&
            !NasClient.isNasUrl(imageKey) &&
            NasClient.instance.isEnabled) {
          triedNasFallback = true;
          final nasBytes = await _tryNasCover(sourceKey, cid);
          if (nasBytes != null) {
            // 缓存到本地，下次直接命中，不再走源站
            await CacheManager().writeCache(cacheKey, nasBytes);
            yield ImageDownloadProgress(
              currentBytes: nasBytes.length,
              totalBytes: nasBytes.length,
              imageBytes: nasBytes,
            );
            return;
          }
        }

        if (retryLimit < 0 || onLoadFailed == null) {
          rethrow;
        }
        var newConfig = await onLoadFailed();
        (configs['onLoadFailed'] as JSInvokable).free();
        onLoadFailed = null;
        if (newConfig == null) {
          rethrow;
        }
        configs = newConfig;
        retryLimit--;
      } finally {
        if (onLoadFailed != null) {
          (configs['onLoadFailed'] as JSInvokable).free();
        }
      }
    }
  }

  /// 尝试从 NAS 取这本漫画的封面字节。
  ///
  /// 返回 null 表示 NAS 没有（未配置/离线/该书未缓存封面）。
  /// 用 /api/v1/comic 探测而不是直接猜 media 路径：
  /// 封面扩展名由落盘时的魔数决定（png/jpg/webp 都有可能），猜不准。
  static Future<Uint8List?> _tryNasCover(String sourceKey, String cid) async {
    try {
      final bytes = await NasClient.instance.fetchNasImage(
        sourceKey: sourceKey,
        comicId: cid,
      );
      return bytes;
    } catch (_) {
      return null;
    }
  }
}

/// A wrapper class for a stream that
/// allows multiple listeners to listen to the same stream.
class _StreamWrapper<T> {
  final Stream<T> _stream;

  final List<StreamController> controllers = [];

  final void Function(_StreamWrapper<T> wrapper) onClosed;

  bool isClosed = false;

  _StreamWrapper(this._stream, this.onClosed) {
    _listen();
  }

  void _listen() async {
    try {
      await for (var data in _stream) {
        if (isClosed) {
          break;
        }
        for (var controller in controllers) {
          if (!controller.isClosed) {
            controller.add(data);
          }
        }
      }
    }
    catch (e) {
      for (var controller in controllers) {
        if (!controller.isClosed) {
          controller.addError(e);
        }
      }
    }
    finally {
      for (var controller in controllers) {
        if (!controller.isClosed) {
          controller.close();
        }
      }
    }
    controllers.clear();
    isClosed = true;
    onClosed(this);
  }

  Stream<T> get stream {
    if (isClosed) {
      throw Exception('Stream is closed');
    }
    var controller = StreamController<T>();
    controllers.add(controller);
    controller.onCancel = () {
      controllers.remove(controller);
    };
    return controller.stream;
  }

  void cancel() {
    for (var controller in controllers) {
      controller.close();
    }
    controllers.clear();
    isClosed = true;
  }
}

class ImageDownloadProgress {
  final int currentBytes;

  final int? totalBytes;

  final Uint8List? imageBytes;

  const ImageDownloadProgress({
    required this.currentBytes,
    required this.totalBytes,
    this.imageBytes,
  });
}
