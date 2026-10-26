import 'package:flutter/foundation.dart';

import 'package:venera/foundation/appdata.dart';
import 'package:venera/foundation/log.dart';
import 'package:venera/foundation/nas_client.dart';

/// 离线 NAS 模式。
///
/// 开关打开后，App 会**优先**从 NAS 取一切可取的资源：
///   - 封面   → NAS 缓存的封面文件
///   - 详情   → NAS 已缓存元信息 + 章节（跳过源站）
///   - 图片   → 阅读器内页（原本就是 NAS 优先，这里强制跳过源站重试）
///
/// 用途是明确的：机场、地铁、异地网络下源站全部打不开，
/// 但 NAS 就在手边（或者通过内网可达）。此时用户想的是
/// "我已经下载过的那些，让我先看"。
///
/// 设计取舍：
///   - 开关是**设备级**设置，不参与跨设备同步——
///     它表达的是"我这台设备现在的网络状况"，不是账号偏好。
///   - 打开时立刻做一次 NAS 可达性探测，不可达直接提示，
///     避免用户以为"模式开了但什么都没变"。
///   - 关闭后行为完全回到原版，不留任何副作用。
class NasOfflineMode with ChangeNotifier {
  NasOfflineMode._();

  static final NasOfflineMode instance = NasOfflineMode._();

  static const _settingKey = 'nasOfflineMode';

  bool _enabled = false;

  bool get enabled => _enabled;

  /// NAS 当前是否真的可用。离线模式开着但 NAS 也连不上时，
  /// UI 要给出明确提示，而不是让用户对着空列表猜。
  bool _nasReachable = false;

  bool get nasReachable => _nasReachable;

  /// 从 appdata 恢复开关状态（启动时调用）。
  void load() {
    try {
      final v = appdata.settings[_settingKey];
      _enabled = v == true;
    } catch (_) {
      _enabled = false;
    }
  }

  /// 切换开关。
  ///
  /// 返回 false 表示"想开但 NAS 不可达"——调用方据此提示用户。
  Future<bool> toggle(bool value) async {
    if (value) {
      if (!NasClient.instance.isEnabled) {
        // 没配 NAS 就没法进离线模式，这是个配置问题，不是错误
        _nasReachable = false;
        return false;
      }
      final ok = await NasClient.instance.probe(force: true);
      _nasReachable = ok;
      if (!ok) return false;
    } else {
      _nasReachable = false;
    }

    _enabled = value;
    appdata.settings[_settingKey] = value;
    try {
      await appdata.saveData(false);
    } catch (e) {
      Log.error('NAS Offline', 'save toggle failed: $e');
    }
    notifyListeners();
    Log.info('NAS Offline', 'mode ${value ? 'enabled' : 'disabled'}');
    return true;
  }

  /// 重新探测 NAS 可达性（模式开启期间定期调用）。
  Future<void> refreshReachability() async {
    if (!_enabled) return;
    final ok = await NasClient.instance.probe(force: true);
    if (ok != _nasReachable) {
      _nasReachable = ok;
      notifyListeners();
    }
  }

  /// 详情页是否应当**跳过源站**直接走 NAS。
  ///
  /// 只在"开关打开且 NAS 可达"时为 true：
  /// 开关开着但 NAS 掉线时，继续尝试源站比直接报错更有用。
  bool get preferNasOnly => _enabled && _nasReachable;

  /// 封面是否应当优先走 NAS（含源站可用时也优先）。
  bool get preferNasCover => _enabled;
}
