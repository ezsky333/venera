# venera-nas

Venera 漫画 App 的 NAS 服务端。用 Go 重写 App 的 JS 漫画源运行环境，
在 NAS 上自动下载收藏夹内容，并提供跨设备增量同步与"NAS 优先"阅读。

```
App (Flutter, fork)  ⇄  venera-nas (Go 单容器)  →  漫画源站点
```

## 它解决什么

| 能力 | 实现 |
|---|---|
| 账号配置 / 收藏 / 进度跨设备同步 | W2 增量协议（操作日志 + 游标），取代原来整包覆盖的 WebDAV |
| 服务端自动下载收藏夹 | goja 跑源脚本 → 批量抓取 → 按 App 目录布局落盘 |
| 阅读优先从 NAS 读 | `GET /api/v1/chapter` 返回可直接加载的图片 URL |
| App 内主动推送到 NAS | `POST /api/v1/download` → 服务端异步任务 |

## 快速开始

```bash
# 1. 起服务（约 25MB 镜像）
VENERA_TOKEN=your-secret docker compose up -d

# 2. 装源（或让 App 同步过来）
cp venera-configs/*.js ./data/sources/
curl -X POST -H "Authorization: Bearer your-secret" \
     http://nas:8080/api/v1/sources/refresh

# 3. 验证
curl http://nas:8080/api/v1/ping
curl -H "Authorization: Bearer your-secret" http://nas:8080/api/v1/info
```

App 侧：**设置 → App → NAS Server**，填地址与 Token，点 Continue。
连接成功后会自动做一次全量对齐，之后所有操作增量同步。

## 架构

```
┌──────────────────────────── NAS ────────────────────────────┐
│                                                             │
│  HTTP 层                                                     │
│   ├─ /api/v1/ping|info           探活/概要                    │
│   ├─ /api/v1/sync/{push,pull,snapshot}   W2 同步             │
│   ├─ /api/v1/chapter             章节缓存查询（阅读优先）     │
│   ├─ /api/v1/media/*             图片直出                    │
│   ├─ /api/v1/download|jobs       下载任务                    │
│   ├─ /api/v1/library             库阅览                       │
│   └─ /webdav/                    W1 兼容（老客户端/迁移）     │
│                                                             │
│  同步引擎    op-log · Lamport · LWW · 游标 · 回显抑制         │
│  任务引擎    SQLite 队列 · worker 池 · 重试/续跑              │
│  源运行时    goja VM（每任务独立）· init.js 桥 · Cookie 隔离  │
│  存储        SQLite（元数据）+ 文件系统（图片）               │
└─────────────────────────────────────────────────────────────┘
```

## 为什么用 Go + goja 而不是复用 Flutter

App 的源运行在 `flutter_qjs`（QuickJS）上，强依赖 Flutter 引擎。
若把 App 做成 Linux 桌面容器，需要 GTK + X11 + WebKit，镜像 500MB+。

实测结论：**`assets/init.js` + venera-configs 全部 35 个源，在 goja 上 35/35 加载成功**，
`loadInfo → loadEp → 图片下载` 全链路可跑通（见 `cmd/probe` 与 `cmd/mocktest`）。

goja 缺少的 `TextDecoder` / `atob` / `Intl` / `Worker`，实测 35 个源**使用量为 0**；
`setTimeout` 由宿主通过 `sendMessage({method:'delay'})` 提供，与 App 语义一致。

## 兼容性关键点

### 1. `sourceKey.hashCode`

App 的收藏/历史表用 `sourceKey.hashCode`（Dart 私有哈希）作为 `type`，
不是字符串。服务端必须复现同一算法，否则永远匹配不上：

```go
// internal/hash — 已用 14 个真实源向量验证
h := uint32(0)
for _, c := range codeUnits(s) { h += c; h += h << 10; h ^= h >> 6 }
h += h << 3; h ^= h >> 11; h += h << 15
return int32(h & 0x3FFFFFFF)
```

因为这是对实现细节的依赖，服务端**同时**维护 `source_type_map` 表，
由客户端首次连接时上报权威值，之后只查表。

### 2. `Network.get` 与 `fetchBytes` 的 body 类型不同

- `Network.get/post/sendRequest` → body 是 **字符串**
- `Network.fetchBytes` / `fetch()` → body 是 **ArrayBuffer**

源大量使用 `JSON.parse(res.body)`，这个差异必须精确复现。

### 3. 章节可能是 JS `Map`

`loadInfo` 返回的 `chapters` 有的是普通对象，有的是 `Map`（如 manga_dex）。
`JSON.stringify(Map)` 会得到 `{}`，必须在 JS 内先判断并转成 entries。
同时 Map 上可能挂附加属性（如 `latestChapterMarker`），不能当成章节。

## 测试

```bash
cd server

# 单元测试
go test ./... -v

# 离线端到端（内置 mock 服务 + 自定义测试源，无需外网）
go run ./cmd/mocktest

# 真实源探测（需外网）
go run ./cmd/probe manga_dex <comicId> --download
```

`cmd/mocktest` 覆盖：JS 源执行 → Map 章节解析 → 干扰属性过滤 →
`getImageLoadingConfig` 头透传 → 并发下载 → 目录布局 → 库索引 → PNG 校验。

## 目录布局

与 App 的 `LocalManager` 完全一致，因此客户端"本地阅读"逻辑可零改动复用：

```
/library/
  <漫画标题> [<id前8位>]/
    cover.png
    <章节ID>/
      0.png
      1.jpg
      ...
```

## 配置

全部通过环境变量，无需挂载配置文件：

| 变量 | 默认 | 说明 |
|---|---|---|
| `VENERA_LISTEN` | `:8080` | 监听地址 |
| `VENERA_DATA_DIR` | `/data` | 元数据/源脚本 |
| `VENERA_LIBRARY_DIR` | `/library` | 漫画文件 |
| `VENERA_TOKEN` | 空 | API 鉴权（空=不鉴权） |
| `VENERA_PUBLIC_BASE_URL` | 空 | 反代场景的外部地址 |
| `VENERA_WORKERS` | `3` | 并发源任务数 |
| `VENERA_DOWNLOAD_CONCURRENCY` | `4` | 单章图片并发 |
| `VENERA_GLOBAL_RPS` | `3` | 每站点限速 |
| `VENERA_AUTO_DOWNLOAD_FAVORITES` | `true` | 收藏后自动排队 |
| `VENERA_WEBDAV_USER/PASS` | venera/venera | WebDAV 兼容凭据 |

## 迁移旧数据

已有的 `.venera` 同步包可直接导入：

```bash
docker run --rm -v $(pwd)/data:/data -v $(pwd)/library:/library \
  -v $(pwd)/old.venera:/tmp/old.venera \
  ghcr.io/venera-app/venera-nas:latest -import -archive /tmp/old.venera
```

或直接把 `.venera` 文件 PUT 到 `/webdav/`，服务端会自动解包导入。

## 已知限制

- **Cloudflare 挑战**：容器内无法过验证。缓解：复用同步包里的 `cf_clearance` Cookie；
  仍失败则该源降级为"仅 App 端下载"。
- **WebView 登录**：`loginWithWebview` 类源无法在服务端登录。
  先在 App 内登录，同步包会带上 `comic_source/*.data` 与 `cookie.db`。
- **`modifyImage` 源**：仅 `jm.js` 使用（长图切片）。已用 Go 图像库实现等价逻辑，
  WebP 输出不支持（输入支持 png/jpg/gif）。
- **源可用性**：站点改版会导致单源失效；采用按源隔离，单源失败不影响其他。

## 许可证

与上游 Venera 一致。仅限个人自用。
