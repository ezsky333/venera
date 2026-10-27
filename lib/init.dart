import 'dart:async';

import 'package:display_mode/display_mode.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:flutter_saf/flutter_saf.dart';
import 'package:rhttp/rhttp.dart';
import 'package:venera/foundation/app.dart';
import 'package:venera/foundation/cache_manager.dart';
import 'package:venera/foundation/comic_source/comic_source.dart';
import 'package:venera/foundation/js_engine.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';
import 'package:venera/foundation/nas_offline_mode.dart';
import 'package:venera/network/cookie_jar.dart';
import 'package:venera/pages/comic_source_page.dart';
import 'package:venera/pages/follow_updates_page.dart';
import 'package:venera/pages/settings/settings_page.dart';
import 'package:venera/utils/app_links.dart';
import 'package:venera/utils/handle_text_share.dart';
import 'package:venera/utils/opencc.dart';
import 'package:venera/utils/nas_sync_notice.dart';
import 'package:venera/utils/nas_source_sync.dart';
import 'package:venera/utils/tags_translation.dart';
import 'package:venera/utils/translations.dart';
import 'foundation/appdata.dart';

extension _FutureInit<T> on Future<T> {
  /// Prevent unhandled exception
  ///
  /// A unhandled exception occurred in init() will cause the app to crash.
  Future<void> wait() async {
    try {
      await this;
    } catch (e, s) {
      Log.error("init", "$e\n$s");
    }
  }
}

Future<void> init() async {
  await App.init().wait();
  await SingleInstanceCookieJar.createInstance();
  // Rust 库 (rhttp) 必须最先初始化完成，否则后续任何使用 AppDio 的
  // 请求都会抛 "flutter_rust_bridge has not been initialized"。
  // 这里单独 await 并显式记录失败原因，避免被下面的 Future.wait 吞掉。
  try {
    await Rhttp.init();
  } catch (e, s) {
    Log.error("Rhttp", "RustLib.init failed: $e\n$s");
    debugPrint("Rhttp init failed: $e\n$s");
  }
  try {
    var futures = [
      App.initComponents(),
      SAFTaskWorker().init().wait(),
      AppTranslation.init().wait(),
      TagsTranslation.readData().wait(),
      JsEngine().init().wait(),
      ComicSourceManager().init().wait(),
      OpenCC.init(),
    ];
    await Future.wait(futures);
  } catch (e, s) {
    Log.error("init", "$e\n$s");
  }
  CacheManager().setLimitSize(appdata.settings['cacheSize']);
  _checkOldConfigs();

  // NAS 服务端：载入配置并在后台做一次增量同步。
  // 失败不影响启动（网络未就绪时很常见），下次操作会重试。
  NasClient.instance.load();
  NasOfflineMode.instance.load();
  if (NasClient.instance.isEnabled) {
    Future.microtask(() async {
      try {
        // 顺序很重要：先补源，再同步数据。
        //
        // 没有源时收藏虽然能落地（用的是 sourceType 而非源对象），
        // 但列表渲染需要源来解析封面/打开详情；先补源能让随后的
        // 同步结果立刻可见，而不是"数据有了但界面还是空的"。
        await NasSourceSync.ensureSourcesOnStartup();
        // 同步一次并提示结果（已是最新 / 同步中 / 同步完成）。
        // 提示要等 OverlayWidget 就绪，内部已处理，这里直接 await。
        await announceNasStartupSync();
      } catch (e) {
        Log.error("NAS", "startup sync failed: $e");
      }
    });
  }

  if (App.isAndroid) {
    handleLinks();
    handleTextShare();
    try {
      await FlutterDisplayMode.setHighRefreshRate();
    } catch(e) {
      Log.error("Display Mode", "Failed to set high refresh rate: $e");
    }
  }
  FlutterError.onError = (details) {
    Log.error("Unhandled Exception", "${details.exception}\n${details.stack}");
  };
  if (App.isWindows) {
    // Report to the monitor thread that the app is running
    // https://github.com/venera-app/venera/issues/343
    Timer.periodic(const Duration(seconds: 1), (_) {
      const methodChannel = MethodChannel('venera/method_channel');
      methodChannel.invokeMethod("heartBeat");
    });
  }
}

void _checkOldConfigs() {
  if (appdata.settings['searchSources'] == null) {
    appdata.settings['searchSources'] = ComicSource.all()
        .where((e) => e.searchPageData != null)
        .map((e) => e.key)
        .toList();
  }

  if (appdata.implicitData['webdavAutoSync'] == null) {
    var webdavConfig = appdata.settings['webdav'];
    if (webdavConfig is List &&
        webdavConfig.length == 3 &&
        webdavConfig.whereType<String>().length == 3) {
      appdata.implicitData['webdavAutoSync'] = true;
    } else {
      appdata.implicitData['webdavAutoSync'] = false;
    }
    appdata.writeImplicitData();
  }

  if (appdata.settings['comicSourceListUrl'].toString().contains("git.nyne.dev")) {
    // migrate to jsdelivr cdn
    appdata.settings['comicSourceListUrl'] = "https://cdn.jsdelivr.net/gh/venera-app/venera-configs@main/index.json";
    appdata.saveData();
  }
}

Future<void> _checkAppUpdates() async {
  var lastCheck = appdata.implicitData['lastCheckUpdate'] ?? 0;
  var now = DateTime.now().millisecondsSinceEpoch;
  if (now - lastCheck < 24 * 60 * 60 * 1000) {
    return;
  }
  appdata.implicitData['lastCheckUpdate'] = now;
  appdata.writeImplicitData();
  ComicSourcePage.checkComicSourceUpdate();
  if (appdata.settings['checkUpdateOnStart']) {
    await checkUpdateUi(false, true);
  }
}

void checkUpdates() {
  _checkAppUpdates();
  FollowUpdatesService.initChecker();
}
