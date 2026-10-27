package jobs

import (
	"sync"
	"sync/atomic"
	"time"
)

// Live 是一个任务的实时进度快照（仅内存，不是数据库记录）。
//
// 为什么需要它：进度是高频变化数据（每张图都在变），
// 如果每次变化都写 SQLite，就要么把频率压到"每秒最多一次"、
// 要么被磁盘 IO 拖垮——这正是面板进度看起来"一卡一卡"的原因。
//
// aria2 的做法是进度全在内存、通过 RPC 实时推送，磁盘只记录
// 任务元信息。这里采用同样的分层：
//   - Live/Hub 负责实时（每张图更新，内存，零 IO）
//   - SQLite 只做检查点（每 5 秒或状态跃变时落一次）
type Live struct {
	JobID int64 `json:"jobId"`
	// Source / ComicID 让客户端能把实时帧关联到具体漫画。
	//
	// 详情页的"NAS 存储面板"只关心**当前这本**漫画的下载进度，
	// 没有这两个字段就只能按 jobId 逐个反查，多一次往返且易错。
	Source  string `json:"source"`
	ComicID string `json:"comicId"`
	// Progress 是整本进度 0..1。
	Progress float64 `json:"progress"`
	// 当前章节信息。
	ChapterIndex int    `json:"chapterIndex"`
	ChapterTotal int    `json:"chapterTotal"`
	ChapterTitle string `json:"chapterTitle"`
	// 当前章节内的图片进度。
	ImageDone  int `json:"imageDone"`
	ImageTotal int `json:"imageTotal"`
	// Bytes 已下载字节数；SpeedBps 是 EWMA 平滑速度（字节/秒）。
	Bytes    int64   `json:"bytes"`
	SpeedBps float64 `json:"speedBps"`
	// Status 与 message 让客户端不必等数据库刷新就能看到状态变化。
	Status  string    `json:"status"`
	Message string    `json:"message"`
	Updated time.Time `json:"updated"`
}

// Hub 保存所有运行中任务的实时进度，并向 SSE 订阅者广播。
//
// 广播是**合并**的：无论一秒内更新多少次，最多每 broadcastInterval
// 推送一帧。这样既能保持 aria2 式的顺滑，又不会让浏览器被帧淹没。
type Hub struct {
	mu   sync.RWMutex
	live map[int64]*Live

	subs map[chan []Live]struct{}

	// dirty 表示内存里有未广播的变化。
	dirty atomic.Bool
	// stateVersion 在任务状态跃变（入队/完成/失败/暂停）时递增。
	// SSE 只在它变化时才重查数据库里的任务列表。
	stateVersion atomic.Int64

	// 速度计算用：上次采样点
	lastSample map[int64]sample

	stop chan struct{}
	once sync.Once
}

type sample struct {
	bytes int64
	at    time.Time
}

// broadcastInterval 是 SSE 推送的最小间隔。
//
// 200ms 已经比人眼需要的更密（aria2 默认约 1s），
// 同时把一帧的序列化成本摊到每秒 5 次以内。
const broadcastInterval = 200 * time.Millisecond

// NewHub 创建并启动广播循环。
func NewHub() *Hub {
	h := &Hub{
		live:       map[int64]*Live{},
		subs:       map[chan []Live]struct{}{},
		lastSample: map[int64]sample{},
		stop:       make(chan struct{}),
	}
	go h.loop()
	return h
}

// Close 停止广播循环（进程退出时调用；测试里也用于清理）。
func (h *Hub) Close() {
	h.once.Do(func() { close(h.stop) })
}

func (h *Hub) loop() {
	t := time.NewTicker(broadcastInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			if !h.dirty.Swap(false) {
				continue
			}
			h.broadcast(h.Snapshot())
		}
	}
}

func (h *Hub) broadcast(frame []Live) {
	h.mu.RLock()
	subs := make([]chan []Live, 0, len(h.subs))
	for c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.RUnlock()
	for _, c := range subs {
		select {
		case c <- frame:
		default:
			// 订阅者消费不过来就丢帧：进度是"最新即正确"的数据，
			// 阻塞在这里会拖慢整个下载。
		}
	}
}

// Subscribe 订阅实时进度帧。返回的 cancel 必须调用，否则连接泄漏。
func (h *Hub) Subscribe() (<-chan []Live, func()) {
	ch := make(chan []Live, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	// 立刻给一帧，客户端不用等第一个 tick
	ch <- h.Snapshot()

	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Snapshot 返回当前所有实时任务的副本。
func (h *Hub) Snapshot() []Live {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Live, 0, len(h.live))
	for _, l := range h.live {
		out = append(out, *l)
	}
	return out
}

// Get 取单个任务的实时状态（不存在时 ok=false）。
func (h *Hub) Get(jobID int64) (Live, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	l, ok := h.live[jobID]
	if !ok {
		return Live{}, false
	}
	return *l, true
}

// Report 更新一个任务的实时进度。
//
// 每张图都会调用，因此实现必须是"内存写 + 原子标记"，
// 不做任何 IO、不加全局锁竞争的临界区。
func (h *Hub) Report(l Live) {
	now := time.Now()
	l.Updated = now

	h.mu.Lock()
	prev, had := h.live[l.JobID]
	if had {
		// 保留已有的字节基线（下载器只汇报增量部分）
		if l.Bytes == 0 {
			l.Bytes = prev.Bytes
		}
		if l.Status == "" {
			l.Status = prev.Status
		}
	}
	// 速度：用两次采样的字节差 / 时间差，再做指数平滑
	if s, ok := h.lastSample[l.JobID]; ok {
		dt := now.Sub(s.at).Seconds()
		if dt > 0.05 {
			delta := float64(l.Bytes - s.bytes)
			if delta < 0 {
				delta = 0
			}
			inst := delta / dt
			if prev != nil && prev.SpeedBps > 0 {
				// 0.7 旧 + 0.3 新：足够跟手又不抖
				l.SpeedBps = prev.SpeedBps*0.7 + inst*0.3
			} else {
				l.SpeedBps = inst
			}
			h.lastSample[l.JobID] = sample{bytes: l.Bytes, at: now}
		} else if prev != nil {
			l.SpeedBps = prev.SpeedBps
		}
	} else {
		h.lastSample[l.JobID] = sample{bytes: l.Bytes, at: now}
	}

	h.live[l.JobID] = &l
	h.mu.Unlock()

	h.dirty.Store(true)
}

// SetStatus 更新任务的实时状态（不改变进度），用于状态跃变。
func (h *Hub) SetStatus(jobID int64, status, message string) {
	h.mu.Lock()
	if l, ok := h.live[jobID]; ok {
		l.Status = status
		l.Message = message
		l.Updated = time.Now()
	} else {
		h.live[jobID] = &Live{JobID: jobID, Status: status, Message: message, Updated: time.Now()}
	}
	h.mu.Unlock()
	h.dirty.Store(true)
	h.BumpState()
}

// Clear 在任务结束后移除实时记录（数据库里已留有最终状态）。
func (h *Hub) Clear(jobID int64) {
	h.mu.Lock()
	delete(h.live, jobID)
	delete(h.lastSample, jobID)
	h.mu.Unlock()
	h.dirty.Store(true)
}

// BumpState 标记"任务状态集合发生了变化"，SSE 据此重查任务列表。
func (h *Hub) BumpState() { h.stateVersion.Add(1) }

// StateVersion 返回当前状态版本号。
func (h *Hub) StateVersion() int64 { return h.stateVersion.Load() }
