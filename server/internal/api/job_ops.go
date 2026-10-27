package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/venera-app/venera-nas/internal/jobs"
	"github.com/venera-app/venera-nas/internal/model"
)

// 本文件实现下载任务的常规管理操作与实时事件推送。
//
// 设计说明：
//   - 操作接口统一 POST /api/v1/jobs/{id}/{action}，动作写在路径里，
//     面板与客户端共用同一套。
//   - 实时更新走 SSE（Server-Sent Events）：下载进度是高频变化的数据，
//     轮询既浪费又延迟；SSE 单向推送正好匹配"服务端产生进度、面板消费"。
//     SSE 用纯 http.Flusher 实现，不引入第三方库，与面板零依赖的取舍一致。

// handleJobAction 处理单个任务的管理动作。
//   POST /api/v1/jobs/{id}/pause | resume | top | untop | cancel | retry
func (s *Server) handleJobAction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	action := r.PathValue("action")
	job, err := s.st.GetJob(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}

	ok := false
	msg := ""
	switch action {
	case "pause":
		var st string
		st, err = s.st.PauseJob(id)
		ok = st != "" || err == nil
		msg = st
	case "resume":
		ok, err = s.st.ResumeJob(id)
		msg = "resumed"
	case "top":
		ok, err = s.st.SetJobPriority(id, 1)
		msg = "topped"
	case "untop":
		ok, err = s.st.SetJobPriority(id, 0)
		msg = "untopped"
	case "cancel":
		ok, err = s.st.CancelJob(id)
		msg = "cancelled"
	case "retry":
		ok, err = s.st.RetryJob(id)
		msg = "requeued"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown action: " + action})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("cannot %s job in status %s", action, job.Status),
		})
		return
	}
	s.log.Info("job action", "id", id, "action", action, "result", msg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action, "result": msg})
}

// handleJobsEvents 是任务事件的 SSE 端点。
//   GET /api/v1/jobs/events
//
// 推送两类事件：
//   - event: live   —— 实时进度帧（内存快照，200ms 合并推送）
//   - event: jobs   —— 任务列表快照（仅在状态集合变化时重查数据库）
//
// 关键改动（性能）：旧实现每秒对每个连接执行一次 Stats()，
// 而 Stats() 要聚合 cached_chapters/favorites/histories 三张大表，
// 多个面板同时打开就是每秒数十次全表聚合。现在进度走内存 Hub，
// 数据库只在"任务状态跃变"时被重查一次。
func (s *Server) handleJobsEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEvent := func(event string, data any) bool {
		b, err := json.Marshal(data)
		if err != nil {
			return true // skip bad payload, keep stream alive
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	statusFilter := r.URL.Query().Get("status")

	// 立即推一帧任务列表，客户端不用等第一个 tick
	sendJobs := func() bool {
		jobs, _, err := s.st.ListJobs(statusFilter, 50, 0)
		if err != nil {
			return true
		}
		return writeEvent("jobs", jobs)
	}
	if !sendJobs() {
		return
	}

	var liveCh <-chan []jobs.Live
	var unsubscribe func()
	if s.hub != nil {
		liveCh, unsubscribe = s.hub.Subscribe()
		defer unsubscribe()
		if !writeEvent("live", s.hub.Snapshot()) {
			return
		}
	}

	// 心跳：防止反代/浏览器在长时间无变化时断流
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// 状态变化兜底轮询：Hub 缺失时退化为每 2 秒查一次
	fallback := time.NewTicker(2 * time.Second)
	defer fallback.Stop()

	seenVersion := int64(-1)
	if s.hub != nil {
		seenVersion = s.hub.StateVersion()
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case frame, ok := <-liveCh:
			if !ok {
				return
			}
			if !writeEvent("live", frame) {
				return
			}
			// 状态跃变（完成/失败/暂停）时补一帧任务列表
			if s.hub != nil {
				if v := s.hub.StateVersion(); v != seenVersion {
					seenVersion = v
					if !sendJobs() {
						return
					}
				}
			}

		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case <-fallback.C:
			// 没有 Hub（或订阅已失效）时保证列表仍能刷新
			if s.hub == nil {
				if !sendJobs() {
					return
				}
				continue
			}
			if v := s.hub.StateVersion(); v != seenVersion {
				seenVersion = v
				if !sendJobs() {
					return
				}
			}
		}
	}
}

// 批量操作：暂停全部 pending / 重试全部 failed（复用现有 store 方法）。
//   POST /api/v1/jobs/bulk  body: {"action": "pause-all"|"resume-all"|"retry-failed"}
func (s *Server) handleJobsBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	switch req.Action {
	case "pause-all":
		ids, err := s.st.JobIDsByStatus(string(model.JobPending))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		n := 0
		for _, id := range ids {
			if _, err := s.st.PauseJob(id); err == nil {
				n++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"affected": n})
	case "resume-all":
		ids, err := s.st.JobIDsByStatus(string(model.JobPaused))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		n := 0
		for _, id := range ids {
			if ok, err := s.st.ResumeJob(id); err == nil && ok {
				n++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"affected": n})
	case "retry-failed":
		n, err := s.st.RetryFailedJobs("")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"affected": n})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown action: " + req.Action})
	}
}
