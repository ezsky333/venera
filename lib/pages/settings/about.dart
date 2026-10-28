part of 'settings_page.dart';

class AboutSettings extends StatefulWidget {
  const AboutSettings({super.key});

  @override
  State<AboutSettings> createState() => _AboutSettingsState();
}

class _AboutSettingsState extends State<AboutSettings> {
  bool isCheckingUpdate = false;

  @override
  Widget build(BuildContext context) {
    return SmoothCustomScrollView(
      slivers: [
        SliverAppbar(title: Text("About".tl)),
        SizedBox(
          height: 112,
          width: double.infinity,
          child: Center(
            child: Container(
              width: 112,
              height: 112,
              decoration: BoxDecoration(
                borderRadius: BorderRadius.circular(136),
              ),
              clipBehavior: Clip.antiAlias,
              child: const Image(
                image: AssetImage("assets/app_icon.png"),
                filterQuality: FilterQuality.medium,
              ),
            ),
          ),
        ).paddingTop(16).toSliver(),
        Column(
          children: [
            const SizedBox(height: 8),
            Text(
              "V${App.version}",
              style: const TextStyle(fontSize: 16),
            ),
            Text("Venera is a free and open-source app for comic reading.".tl),
            const SizedBox(height: 8),
          ],
        ).toSliver(),
        ListTile(
          title: Text("Check for updates".tl),
          trailing: Button.filled(
            isLoading: isCheckingUpdate,
            child: Text("Check".tl),
            onPressed: () {
              setState(() {
                isCheckingUpdate = true;
              });
              checkUpdateUi().then((value) {
                setState(() {
                  isCheckingUpdate = false;
                });
              });
            },
          ).fixHeight(32),
        ).toSliver(),
        _SwitchSetting(
          title: "Check for updates on startup".tl,
          settingKey: "checkUpdateOnStart",
        ).toSliver(),
        ListTile(
          title: const Text("Github"),
          trailing: const Icon(Icons.open_in_new),
          onTap: () {
            // 指向本 fork：这里的版本才带 NAS 那套改动，
            // 把用户送去上游仓库等于引导他装回原版。
            launchUrlString("https://github.com/$updateRepo");
          },
        ).toSliver(),
        ListTile(
          title: const Text("Telegram"),
          trailing: const Icon(Icons.open_in_new),
          onTap: () {
            launchUrlString("https://t.me/venera_release");
          },
        ).toSliver(),
      ],
    );
  }
}

Future<void> checkUpdateUi([bool showMessageIfNoUpdate = true, bool delay = false]) async {
  try {
    final info = await fetchLatestRelease();
    if (info == null) {
      // 查不到 ≠ 没有新版。原来这两种情况都走"没有新版本可用"，
      // 用户在网络不好时会得到一句确定的假话。这里如实说查不到。
      if (showMessageIfNoUpdate) {
        App.rootContext.showMessage(message: "Failed to check for updates".tl);
      }
      return;
    }
    if (!isNewerVersion(info.version, App.version)) {
      if (showMessageIfNoUpdate) {
        App.rootContext.showMessage(message: "No new version available".tl);
      }
      return;
    }
    if (delay) {
      await Future.delayed(const Duration(seconds: 2));
    }
    showDialog(
        context: App.rootContext,
        builder: (context) {
          return ContentDialog(
            title: "New version available".tl,
            content: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  "A new version is available. Do you want to update now?".tl,
                ),
                const SizedBox(height: 8),
                Text(
                  "${App.version} → ${info.version}",
                  style: const TextStyle(fontSize: 12),
                ),
              ],
            ).paddingHorizontal(16),
            actions: [
              Button.text(
                onPressed: () {
                  Navigator.pop(context);
                  launchUrlString(info.url);
                },
                child: Text("Update".tl),
              ),
            ],
          );
        });
  } catch (e, s) {
    Log.error("Check Update", e.toString(), s);
  }
}
