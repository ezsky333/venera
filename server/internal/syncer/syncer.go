// Package syncer 实现 W2 增量同步协议。
//
// 设计要点（与 App 现有 WebDAV"整包覆盖"的根本区别）：
//
//  1. 操作日志（op log）而非快照：客户端只上传"我做了什么"，
//     服务端线性化后广播给其他设备。天然支持多设备并发。
//  2. 游标增量拉取：客户端记录最后见到的 op id，重连后只需拉取
//     该 id 之后的操作，不必下载整包。
//  3. 语义化合并：
//     - 收藏是集合语义 → 求并集，删除用墓碑
//     - 阅读进度是 LWW → 按客户端时间取最新
//     - 源私有数据按 key 合并，不整体覆盖
//  4. 回显抑制：客户端不会收到自己刚提交的操作，避免抖动。
//  5. 丢弃旧操作的防护：所有写操作带 Lamport 时钟，
//     旧操作不会覆盖新状态。
package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
)

// Syncer 是同步协议的实现。
type Syncer struct {
	st *store.Store
	// OnNewFavorites 在收藏新增后被调用（用于自动排队下载）。
	OnNewFavorites func(items []model.Favorite) error
	// OnSourceScript 在收到源脚本时调用（落到磁盘 + 刷新源管理器）。
	OnSourceScript func(payload model.SourceScriptOpPayload) error
	// OnSourceRemove 在客户端卸载源时调用。
	OnSourceRemove func(sourceKey string) error
	// OnSourceCredentials 在该源的登录态（data 或 cookie）落库后调用。
	//
	// 用途：把"等登录态"期间推迟的下载任务放回队列。没有这个回调，
	// 客户端连上后先落收藏、任务立刻开跑并因无 Cookie 被推迟，
	// 之后 Cookie 到了也没人叫醒它们，只能等用户手动重试。
	OnSourceCredentials func(sourceKey string) error
	// ListInstalledSources 返回服务端当前实际持有的源（磁盘扫描结果）。
	// 快照用它作答，保证客户端看到的源清单与真实可用的一致。
	ListInstalledSources func() []store.SourceInfo
}

// New 创建 Syncer。
func New(st *store.Store) *Syncer {
	return &Syncer{st: st}
}

// PushRequest 是客户端提交的一批操作。
type PushRequest struct {
	ClientID string `json:"clientId"`
	// DeviceName 仅用于运维时识别设备。
	DeviceName string `json:"deviceName,omitempty"`
	// Lamport 是客户端当前的逻辑时钟。服务端据此分配单调序号。
	Lamport int64 `json:"lamport"`
	// Ops 是本批操作，按产生顺序排列。
	Ops []PushOp `json:"ops"`
}

// PushOp 是单条待提交操作。
type PushOp struct {
	Type string `json:"type"`
	// Lamport 可选：若客户端能提供，用于同类操作的因果排序。
	Lamport int64 `json:"lamport,omitempty"`
	Payload any   `json:"payload"`
}

// PushResponse 返回提交结果与新游标。
type PushResponse struct {
	// Applied 是成功落库的操作数。
	Applied int `json:"applied"`
	// Cursor 是服务端当前最大 op id，客户端应保存它。
	Cursor int64 `json:"cursor"`
	// ServerLamport 让客户端可以校准自己的逻辑时钟。
	ServerLamport int64 `json:"serverLamport"`
	// Warnings 记录被拒绝/忽略的操作（如引用不存在的收藏夹）。
	Warnings []string `json:"warnings,omitempty"`
}

// Push 应用一批客户端操作。
func (s *Syncer) Push(req PushRequest) (*PushResponse, error) {
	if req.ClientID == "" {
		return nil, errors.New("clientId is required")
	}
	resp := &PushResponse{}
	var newFavs []model.Favorite

	for _, op := range req.Ops {
		lamport := op.Lamport
		if lamport <= 0 {
			lamport = req.Lamport
		}
		if lamport <= 0 {
			lamport = time.Now().UnixMilli()
		}

		applied, warn, fav, err := s.apply(req.ClientID, op, lamport)
		if err != nil {
			// 单条失败不应中断整批（否则一个坏操作会阻塞所有同步）
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("%s: %v", op.Type, err))
			continue
		}
		if warn != "" {
			resp.Warnings = append(resp.Warnings, warn)
		}
		if applied {
			resp.Applied++
			// 只有真正落库的操作才进入日志，保证广播一致性
			if _, err := s.st.AppendOp(req.ClientID, op.Type, lamport, op.Payload); err != nil {
				return nil, err
			}
		}
		if fav != nil {
			newFavs = append(newFavs, *fav)
		}
	}

	cursor, err := s.st.MaxOpID()
	if err != nil {
		return nil, err
	}
	resp.Cursor = cursor
	resp.ServerLamport = time.Now().UnixMilli()

	_ = s.st.TouchClient(req.ClientID, req.DeviceName, cursor)

	// 收藏新增 → 排队下载（可关闭）
	if len(newFavs) > 0 && s.OnNewFavorites != nil {
		if err := s.OnNewFavorites(newFavs); err != nil {
			resp.Warnings = append(resp.Warnings, "queue download: "+err.Error())
		}
	}
	return resp, nil
}

// apply 把一条操作合入服务端状态。
// 返回 (是否真实改变了状态, 警告信息, 新增的收藏, 错误)。
func (s *Syncer) apply(clientID string, op PushOp, lamport int64) (bool, string, *model.Favorite, error) {
	switch model.OpType(op.Type) {
	case model.OpFavoriteAdd:
		payload, err := decodePayload[model.FavoriteOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Folder == "" {
			return false, "favorite add ignored: empty folder", nil, nil
		}
		sourceType := payload.SourceType
		if sourceType == 0 {
			sourceType = s.st.SourceType(payload.Source, hashOf(payload.Source))
		}
		fav := model.Favorite{
			Comic: model.Comic{
				Source: payload.Source, ID: payload.ComicID,
				Title: payload.Title, Cover: payload.Cover, Tags: payload.Tags,
			},
			Folder: payload.Folder, Author: payload.Author,
			AddedAt: time.Now(), SourceType: sourceType,
		}
		// 顺序必须原样带过来：客户端列表按 display_order 升序展示，
		// 缺这个值时接收端会按"新条目插到开头"处理，整批顺序被颠倒。
		if payload.Order != nil {
			fav.DisplayOrder = *payload.Order
		}
		changed, err := s.st.AddFavorite(fav, lamport)
		if err != nil {
			if errors.Is(err, store.ErrFavoriteResurrect) {
				// 这条 add 比墓碑更旧：用户已经删掉了它。
				// 不能静默丢弃——推送方本地还留着这条收藏（它是从本地
				// 队列里发出来的），必须让它知道要删掉，否则那台设备上
				// 会一直留着一个别的设备都看不到的幽灵收藏。
				return false,
					fmt.Sprintf("favorite resurrect blocked: %s/%s/%s",
						payload.Folder, payload.Source, payload.ComicID),
					nil, nil
			}
			return false, "", nil, err
		}
		// 顺带缓存漫画元信息
		_ = s.st.UpsertComic(fav.Comic, sourceType)
		return changed, "", &fav, nil

	case model.OpFavoriteRemove:
		payload, err := decodePayload[model.FavoriteOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		changed, err := s.st.RemoveFavorite(payload.Folder, payload.Source, payload.ComicID, lamport)
		if err != nil {
			return false, "", nil, err
		}
		return changed, "", nil, nil

	case model.OpFavoriteMove:
		payload, err := decodePayload[model.FavoriteMovePayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		sourceType := payload.SourceType
		if sourceType == 0 {
			sourceType = s.st.SourceType(payload.Source, hashOf(payload.Source))
		}
		fav := model.Favorite{
			Comic: model.Comic{
				Source: payload.Source, ID: payload.ComicID,
				Title: payload.Title, Cover: payload.Cover, Tags: payload.Tags,
			},
			Folder: payload.ToFolder, Author: payload.Author,
			AddedAt: time.Now(), SourceType: sourceType,
		}
		if err := s.st.MoveFavorite(fav, payload.FromFolder, lamport); err != nil {
			return false, "", nil, err
		}
		return true, "", &fav, nil

	case model.OpHistoryUpdate:
		payload, err := decodePayload[model.HistoryOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		sourceType := payload.SourceType
		if sourceType == 0 {
			sourceType = s.st.SourceType(payload.Source, hashOf(payload.Source))
		}
		at := time.Now()
		if payload.At > 0 {
			at = time.UnixMilli(payload.At)
		}
		h := model.History{
			Comic: model.Comic{
				Source: payload.Source, ID: payload.ComicID,
				Title: payload.Title, Cover: payload.Cover,
			},
			SourceType: sourceType,
			Ep:         payload.Ep, Page: payload.Page, Group: payload.Group,
			ReadEps: payload.ReadEps, MaxPage: payload.MaxPage,
			UpdatedAt: at,
		}
		changed, err := s.st.UpsertHistory(h)
		if err != nil {
			return false, "", nil, err
		}
		_ = s.st.UpsertComic(h.Comic, sourceType)
		return changed, "", nil, nil

	case model.OpSourceData:
		payload, err := decodePayload[model.SourceDataOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Source == "" {
			return false, "source data ignored: empty source", nil, nil
		}
		if err := s.st.MergeSourceData(payload.Source, payload.Data); err != nil {
			return false, "", nil, err
		}
		if s.OnSourceCredentials != nil {
			if err := s.OnSourceCredentials(payload.Source); err != nil {
				return true, "credentials hook: " + err.Error(), nil, nil
			}
		}
		return true, "", nil, nil

	case model.OpSourceCookie:
		payload, err := decodePayload[model.SourceCookieOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Source == "" {
			return false, "source cookie ignored: empty source", nil, nil
		}
		if err := s.st.PutCookies(payload.Source, payload.Cookies); err != nil {
			return false, "", nil, err
		}
		if s.OnSourceCredentials != nil {
			if err := s.OnSourceCredentials(payload.Source); err != nil {
				return true, "credentials hook: " + err.Error(), nil, nil
			}
		}
		return true, "", nil, nil

	case model.OpSettingUpdate:
		payload, err := decodePayload[model.SettingOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Key == "" {
			return false, "setting ignored: empty key", nil, nil
		}
		// 空集合保护：不要让一台"刚装好、什么都还没配"的设备
		// 用空列表覆盖另一台已经配好的设备。
		//
		// 真实踩过的坑：pad 首次连接 → 本地 explore_pages/categories
		// 都是空数组 → _bootstrapPush 把它们当"我的设置"推上来
		// → 服务端原有的 ["Picacg","禁漫天堂","E-Hell"] 被清空
		// → 手机下次同步看到空列表，探索页也空了。
		//
		// 判据：值为空数组/空字符串/空 map，且服务端已有非空值时，忽略。
		if isEmptySettingValue(payload.Value) {
			var existing any
			if ok, _ := s.st.GetSetting(payload.Key, &existing); ok {
				if existing != nil && !isEmptySettingValue(existing) {
					return false, "setting ignored: refuse to overwrite non-empty value with empty (" + payload.Key + ")", nil, nil
				}
			}
		}
		if err := s.st.PutSetting(payload.Key, payload.Value); err != nil {
			return false, "", nil, err
		}
		return true, "", nil, nil

	case model.OpSourceScript:
		payload, err := decodePayload[model.SourceScriptOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Source == "" || payload.Script == "" {
			return false, "source script ignored: empty source or script", nil, nil
		}
		// 交给上层（main 注入的回调）把脚本落到磁盘并刷新源管理器。
		// syncer 本身不碰文件系统，保持职责单一、便于测试。
		if s.OnSourceScript != nil {
			if err := s.OnSourceScript(payload); err != nil {
				return false, "", nil, err
			}
		}
		return true, "", nil, nil

	case model.OpSourceRemove:
		payload, err := decodePayload[model.SourceRemoveOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Source == "" {
			return false, "source remove ignored: empty source", nil, nil
		}
		if s.OnSourceRemove != nil {
			if err := s.OnSourceRemove(payload.Source); err != nil {
				return false, "", nil, err
			}
		}
		return true, "", nil, nil

	case model.OpSourceInstall:
		payload, err := decodePayload[model.SourceInstallOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Source == "" {
			return false, "source install ignored: empty source", nil, nil
		}
		// 服务端可能已经有这个源（另一个设备早先同步过），
		// 这时只需记录"该设备装了这个源"，无需重复写脚本。
		return true, "", nil, nil
	}
	return false, fmt.Sprintf("unknown op type %q", op.Type), nil, nil
}

// PullRequest 是客户端拉取请求。
type PullRequest struct {
	ClientID string
	Cursor   int64
	Limit    int
	// IncludeSelf 为 true 时也返回该客户端自己的操作（全量恢复场景）。
	IncludeSelf bool
}

// PullResponse 是拉取结果。
type PullResponse struct {
	Ops    []model.Op `json:"ops"`
	Cursor int64      `json:"cursor"`
	// HasMore 表示还有更多操作，客户端应继续拉取。
	HasMore bool `json:"hasMore"`
	// ServerLamport 用于客户端校准逻辑时钟。
	ServerLamport int64 `json:"serverLamport"`
	// ResyncRequired 表示请求的游标已过期（操作被裁剪），客户端必须全量重来。
	ResyncRequired bool `json:"resyncRequired"`
	// Snapshot 在全量恢复时直接给出当前状态，避免回放全部历史。
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// Pull 返回游标之后的操作。
func (s *Syncer) Pull(req PullRequest) (*PullResponse, error) {
	exclude := req.ClientID
	if req.IncludeSelf {
		exclude = ""
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 500
	}

	ops, err := s.st.OpsSince(req.Cursor, limit, exclude)
	if err != nil {
		return nil, err
	}
	maxID, err := s.st.MaxOpID()
	if err != nil {
		return nil, err
	}

	resp := &PullResponse{
		Ops:           ops,
		Cursor:        maxID,
		HasMore:       len(ops) == limit && (len(ops) > 0 && ops[len(ops)-1].ID < maxID),
		ServerLamport: time.Now().UnixMilli(),
	}
	if len(ops) > 0 {
		resp.Cursor = ops[len(ops)-1].ID
	}
	return resp, nil
}

// SourceCredentials 是只包含"源登录态"的轻量响应。
//
// 单独做一个端点而不是复用快照：快照含 500 条历史 + 全部收藏（几百 KB），
// 而"补登录态"只需要几十 KB。增量同步路径（游标已存在）不会走
// _pullSnapshot，但新装的源、或本地 data 被清掉的设备仍然需要这份数据，
// 所以必须有个便宜的入口。
//
// 现场教训：客户端早就在调这个端点，服务端却一直 404（端点只写在了
// 一个没进仓库的副本里），于是增量路径永远补不上 Cookie，
// 表现为"pica 账号没同步过来"、"ehentai 详情 302"。
type SourceCredentials struct {
	Data    map[string]map[string]any       `json:"data,omitempty"`
	Cookies map[string][]model.CookieRecord `json:"cookies,omitempty"`
}

// BuildSourceCredentials 生成源登录态快照。
func (s *Syncer) BuildSourceCredentials() (*SourceCredentials, error) {
	out := &SourceCredentials{}
	if data, err := s.st.AllSourceData(); err == nil && len(data) > 0 {
		out.Data = data
	}
	if cookies, err := s.st.AllCookies(); err == nil && len(cookies) > 0 {
		out.Cookies = cookies
	}
	return out, nil
}

// Snapshot 是服务端当前状态的完整快照，用于客户端首次接入或游标过期后的重建。
type Snapshot struct {
	Favorites map[string][]FavoriteView  `json:"favorites"`
	Histories []HistoryView              `json:"histories"`
	Settings  map[string]json.RawMessage `json:"settings,omitempty"`
	Sources   []store.SourceInfo         `json:"sources,omitempty"`
	// SourceData 是各源的私有配置（账号、token、baseUrl 等）。
	//
	// 必须进快照：新设备首次连接走的就是快照路径（NasSync.syncNow(full:true)
	// → _pullSnapshot），而 source.data 的增量 op 只会被 _pullIncremental
	// 读到。早先快照漏了这个字段，于是新设备连上后 source.data 永远为空。
	//
	// 实际症状：服务端存着 picacg 的 394 字节配置（account + JWT token），
	// 新设备却拿不到 —— 用户看到的是"pica 的凭证没同步过来"，实际是
	// 客户端根本没拿到，而不是没写入。
	SourceData map[string]map[string]any `json:"sourceData,omitempty"`
	// SourceCookies 是各源的登录 Cookie（按源分组）。
	//
	// 与 SourceData 同理，且更关键：很多源（ehentai / jm / wnacg）的
	// 登录态**只**存在于 Cookie 里，source.data 是空壳。快照不带这份
	// 数据，新设备上的源就是未登录状态。
	SourceCookies map[string][]model.CookieRecord `json:"sourceCookies,omitempty"`
	Cursor        int64                           `json:"cursor"`
	At            time.Time                       `json:"at"`
}

// FavoriteView 是发给客户端的收藏条目。
//
// 相比 model.Favorite 额外带两个字段：
//   - sourceType：客户端在**尚未安装漫画源**时也要能把收藏正确落地
//     （用的是同一套 hashCode，装源后自动对上）。
//   - comicId：内嵌的 Comic 把 id 序列化成 `id`，但客户端在增量 op 里
//     读的是 `comicId`。两个路径用不同名字是历史遗留，
//     这里显式再给一份 `comicId`，让客户端两种写法都能用，
//     避免"快照一条都应用不上"这种排查成本极高的 bug
//     （真实发生过：449 条收藏全失败，且不抛异常）。
type FavoriteView struct {
	model.Favorite
	SourceKey string `json:"sourceKey"`
	ComicID   string `json:"comicId"`
}

// HistoryView 同理。
type HistoryView struct {
	model.History
	SourceKey string `json:"sourceKey"`
	ComicID   string `json:"comicId"`
	// At 是更新时间的毫秒时间戳。
	//
	// 必须显式给出：内嵌的 model.History 把时间序列化成 `updatedAt`
	// （RFC3339 字符串），而客户端做 LWW 比较时读的是 `at`（int 毫秒）。
	// 字段名对不上，客户端拿到的 remoteTime 恒为 0，
	// 于是"本地已有记录 >= 0" 永远成立，所有历史都被静默跳过。
	// 这正是"收藏同步过来了、历史却一条都没有"的原因。
	At int64 `json:"at"`
}

// BuildSnapshot 生成完整快照。
func (s *Syncer) BuildSnapshot() (*Snapshot, error) {
	all, err := s.st.AllFavoriteComics()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{
		Favorites: map[string][]FavoriteView{},
		At:        time.Now(),
	}
	for _, f := range all {
		st := f.SourceType
		if st == 0 {
			st = s.st.SourceType(f.Source, hashOf(f.Source))
		}
		f.SourceType = st
		snap.Favorites[f.Folder] = append(snap.Favorites[f.Folder], FavoriteView{
			Favorite:  f,
			SourceKey: f.Source,
			ComicID:   f.ID,
		})
	}
	// limit 传 0 = 全部历史。快照是"新设备唯一的一次全量机会"：
	// 截断多少条，接收端就永久少多少条（游标会被推到最新，
	// 那些旧 op 再也不会重放）。
	hist, err := s.st.ListHistories(0)
	if err != nil {
		return nil, err
	}
	for _, h := range hist {
		st := h.SourceType
		if st == 0 {
			st = s.st.SourceType(h.Source, hashOf(h.Source))
		}
		h.SourceType = st
		snap.Histories = append(snap.Histories, HistoryView{
			History:   h,
			SourceKey: h.Source,
			ComicID:   h.ID,
			At:        h.UpdatedAt.UnixMilli(),
		})
	}
	settings, err := s.st.AllSettings()
	if err != nil {
		return nil, err
	}
	// 剔除设备特有配置：这些项从一个设备搬到另一个设备是有害的。
	//
	// 判断标准（与用户确认过的原则一致）：
	//   - 只排除"跟着这台机器走"的东西（代理、设备 ID、本机路径…）；
	//   - 其余一律同步。多设备是同一个人在用，
	//     "我在手机上关了某个源，平板上应该也关掉"才是预期。
	for _, k := range DeviceOnlySettings {
		delete(settings, k)
	}
	snap.Settings = settings
	srcs, err := s.st.ListSources()
	if err != nil {
		return nil, err
	}
	// 磁盘扫描出来的源才是真相（sources 表只记录过客户端上报的元信息）。
	// 用 SourceLister 覆盖，保证快照里的源清单与 /api/v1/sources 一致。
	if s.ListInstalledSources != nil {
		if live := s.ListInstalledSources(); len(live) > 0 {
			srcs = live
		}
	}
	snap.Sources = srcs

	// ---- 源私有配置与 Cookie ----
	//
	// 这是"新设备登录态为零"的根因修复点。客户端 full sync 只读快照，
	// 而快照此前完全没有这两个字段，于是：
	//   - picacg：token 在 source_data 里，新设备拿不到 → 未登录；
	//   - ehentai / jm / wnacg：登录态在 Cookie 里 → 同样拿不到。
	//
	// 失败不阻断整个快照：源配置缺失只是"要重新登录"，
	// 而快照失败是"收藏和历史都没有"，后者严重得多。
	if data, err := s.st.AllSourceData(); err == nil && len(data) > 0 {
		snap.SourceData = data
	}
	if cookies, err := s.st.AllCookies(); err == nil && len(cookies) > 0 {
		snap.SourceCookies = cookies
	}

	cursor, err := s.st.MaxOpID()
	if err != nil {
		return nil, err
	}
	snap.Cursor = cursor
	return snap, nil
}

// DeviceOnlySettings 是**不参与跨设备同步**的配置项。
//
// 与 App 侧 appdata._disableSync 保持同一套语义，并补上 NAS 自身相关的项：
//
//	proxy                  代理地址通常绑定到具体网络环境
//	authorizationRequired  本机生物识别开关
//	customImageProcessing  可能引用本机脚本路径
//	webdav                 WebDAV 账号密码（各设备可不同）
//	disableSyncFields      同步排除清单本身
//	deviceId               设备标识
//	nas                    NAS 服务端地址与 token（每台设备各自配置）
//	nasClientId            设备级客户端 ID
var DeviceOnlySettings = []string{
	"proxy",
	"authorizationRequired",
	"customImageProcessing",
	"webdav",
	"disableSyncFields",
	"deviceId",
	"nas",
	"nasClientId",
}

// IsDeviceOnlySetting 判断某个键是否属于设备特有配置。
func IsDeviceOnlySetting(key string) bool {
	for _, k := range DeviceOnlySettings {
		if k == key {
			return true
		}
	}
	return false
}

// isEmptySettingValue 判断一个设置值是否"空"。
//
// 只把**结构性为空**的东西算空：空数组、空字符串、空 map、nil。
// 刻意不把 `false` / `0` 当成空 —— 那些是有意义的用户选择
// （例如 reverseChapterOrder=false 是"不要倒序"，不能拿它当"未设置"）。
func isEmptySettingValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// decodePayload 把 any 重新编码/解码成具体类型。
// 这样调用方可以两者皆传：解析后的 struct，或 json.RawMessage。
func decodePayload[T any](payload any) (T, error) {
	var out T
	switch v := payload.(type) {
	case nil:
		return out, fmt.Errorf("empty payload")
	case json.RawMessage:
		return out, json.Unmarshal(v, &out)
	case []byte:
		return out, json.Unmarshal(v, &out)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}
