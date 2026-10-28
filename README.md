# venera（NAS 分支）

[![flutter](https://img.shields.io/badge/flutter-3.47.7-blue)](https://flutter.dev/)
[![License](https://img.shields.io/github/license/ezsky333/venera)](https://github.com/ezsky333/venera/blob/master/LICENSE)
[![Release](https://img.shields.io/github/v/release/ezsky333/venera)](https://github.com/ezsky333/venera/releases)
[![Docker Image](https://img.shields.io/badge/ghcr.io-ezsky333%2Fvenera--nas-1f883d)](https://github.com/ezsky333/venera/pkgs/container/venera-nas)

一个漫画阅读器，上游是 [venera-app/venera](https://github.com/venera-app/venera)。本分支在它之上加了一套 **NAS 服务端，作为缓存中台**：下载任务由 NAS 接管，收藏、阅读进度、源账号在多设备间保持同步。手机和平板不再各自下载资源，直接读 NAS 缓存——**一处下载，多设备共用**。

## 用起来是什么样

![多设备共用一台 NAS](docs/assets/multi-device.svg)

- **同步**：任何设备上收藏、看到哪一页，其他设备打开就是最新的
- **下载**：新收藏自动在 NAS 排队下载；手动想下的，详情页点「下载到 NAS」
- **阅读**：本地缓存 → NAS 缓存 → 源站，按顺序取，NAS 有的章节不用再等网络
- **省存储**：几百 GB 的漫画库放在 NAS，设备端只留一点本地缓存

## 三步上手

**1. 起服务端**（NAS、软路由、闲置电脑，有 Docker 就行）：

```bash
mkdir -p data library
docker run -d --name venera-nas \
  -p 8080:8080 \
  -v ./data:/data \
  -v ./library:/library \
  -e VENERA_TOKEN=换成你的token \
  ghcr.io/ezsky333/venera-nas:latest
```

<details>
<summary>常用环境变量（全部可选）</summary>

| 变量 | 说明 |
|---|---|
| `VENERA_LISTEN` | 监听地址，默认 `:8080` |
| `VENERA_DATA_DIR` / `VENERA_LIBRARY_DIR` | 元数据 / 漫画文件目录 |
| `VENERA_TOKEN` | 访问令牌，客户端填同一个值 |
| `VENERA_PROXY` | 出站代理（访问源站用） |
| `VENERA_WORKERS` / `VENERA_DOWNLOAD_CONCURRENCY` | 任务并发 / 单任务图片并发 |
| `VENERA_AUTO_DOWNLOAD_FAVORITES` | 收藏到货自动排队下载（默认开） |

</details>

**2. 客户端连接**：设置 → 應用 → NAS 伺服器，填地址和 token。首次连接会把本地的收藏、历史、源账号自动推上去。

![NAS 设置页](docs/assets/nas-settings.png)

**3. 之后正常用**：该搜搜、该看看。管理面板在 `http://<nas>:8080/admin`——库存、任务、收藏夹都在上面，失败任务可重试，还有一个「补全元数据信息」按钮补作者简介。

## 离线模式

首页顶部的「离线 NAS 模式」开关，决定 **NAS 缓存之外的请求还发不发**：

![离线模式](docs/assets/offline-mode.svg)

默认关。日常在家、网络顺畅，保持关闭即可——三级兜底自动工作，NAS 缓存先顶上，源站只兜最底。

人在外面、通过内网穿透（DDNS / frp / Tailscale 均可）访问家里 NAS 时，打开它：页面路径在 NAS 上闭环，不向源站发任何请求，不用折腾代理和超时。

## 原理（想折腾再读）

服务端是 Go 单二进制（Docker 镜像 amd64/arm64），comic 源还是那些 JS 脚本——服务端里用 goja 跑它们，客户端装的源会自动同步上去，登录态也一起带过去。文件落盘布局与 App 本地下载完全一致，客户端可以直接当"本地漫画"读。

![整体结构](docs/assets/architecture.svg)

### 同步协议 W2

上游的 WebDAV 同步是把数据库打成 zip 整包上传，多设备必然互相覆盖。本分支实现新协议 W2：只传增量操作，服务端按语义合并。

![W2 同步协议](docs/assets/sync-flow.svg)

- **幂等**，重复推同一条收藏不会出现两条
- **收藏是集合语义**——删除写墓碑表，迟到的旧 add 复活不了它
- **历史是 LWW**，按客户端时间戳取新
- **凭证先于收藏落地**，服务端拿到 Cookie 之后才排队下载

协议细节见 [docs/03-W2同步协议规范.md](docs/03-W2同步协议规范.md)。

### 数据完整性

- 源站返回 0 页的章节不再被记成"已缓存"
- 下载中途失败的章节整个作废重下，不留编号空洞（真案例：一章 page_count=64 但磁盘缺 6 页，按序号拼 URL 就 404）
- `maintenance/page-audit` 端点核对 page_count 与磁盘文件数

## 从源码构建

1. 装 Flutter（版本锁在 [pubspec.yaml](pubspec.yaml)）和 Rust
2. `flutter build apk`（或对应平台）
3. 服务端：`cd server && go build ./cmd/venera-nas`

## 文档

| 文档 | 内容 |
|---|---|
| [01-架构分析.md](docs/01-架构分析.md) | 上游架构、源脚本宿主 API 的实测分布、可行性判断 |
| [02-技术方案与决策.md](docs/02-技术方案与决策.md) | 为什么 Go + goja、为什么不用 Flutter headless |
| [03-W2同步协议规范.md](docs/03-W2同步协议规范.md) | op 类型、合并语义、游标、端点契约 |
| [04-部署指南.md](docs/04-部署指南.md) | Docker / compose / 卷权限 / 反代 |
| [05-改动清单.md](docs/05-改动清单.md) | 相对上游改了哪些文件、加了哪些 |

## 与上游的差异边界

- 独立包名 `xyz.ezsky.venera`，可与上游共存
- CI 全平台出包（Android / Windows / macOS / Linux / iOS），tag 推送即发布
- 上游的 WebDAV 同步保留在 `/webdav/`，老数据可以先用它迁移再切 W2

## 致谢

- [venera-app/venera](https://github.com/venera-app/venera) —— 本分支的全部基础
- [EhTagTranslation](https://github.com/EhTagTranslation/Database) —— 标签中文翻译
- 源脚本来自社区维护的 [venera-configs](https://github.com/venera-app/venera-configs)

## License

GPL-3.0，与上游一致。
