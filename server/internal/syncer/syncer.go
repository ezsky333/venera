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
		changed, err := s.st.AddFavorite(fav, lamport)
		if err != nil {
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
		changed, err := s.st.RemoveFavorite(payload.Folder, payload.Source, payload.ComicID)
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
		return true, "", nil, nil

	case model.OpSettingUpdate:
		payload, err := decodePayload[model.SettingOpPayload](op.Payload)
		if err != nil {
			return false, "", nil, err
		}
		if payload.Key == "" {
			return false, "setting ignored: empty key", nil, nil
		}
		if err := s.st.PutSetting(payload.Key, payload.Value); err != nil {
			return false, "", nil, err
		}
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

// Snapshot 是服务端当前状态的完整快照，用于客户端首次接入或游标过期后的重建。
type Snapshot struct {
	Favorites map[string][]model.Favorite `json:"favorites"`
	Histories []model.History             `json:"histories"`
	Settings  map[string]json.RawMessage  `json:"settings,omitempty"`
	Sources   []store.SourceInfo          `json:"sources,omitempty"`
	Cursor    int64                       `json:"cursor"`
	At        time.Time                   `json:"at"`
}

// BuildSnapshot 生成完整快照。
func (s *Syncer) BuildSnapshot() (*Snapshot, error) {
	all, err := s.st.AllFavoriteComics()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{
		Favorites: map[string][]model.Favorite{},
		At:        time.Now(),
	}
	for _, f := range all {
		snap.Favorites[f.Folder] = append(snap.Favorites[f.Folder], f)
	}
	hist, err := s.st.ListHistories(500)
	if err != nil {
		return nil, err
	}
	snap.Histories = hist
	settings, err := s.st.AllSettings()
	if err != nil {
		return nil, err
	}
	snap.Settings = settings
	srcs, err := s.st.ListSources()
	if err != nil {
		return nil, err
	}
	snap.Sources = srcs
	cursor, err := s.st.MaxOpID()
	if err != nil {
		return nil, err
	}
	snap.Cursor = cursor
	return snap, nil
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
