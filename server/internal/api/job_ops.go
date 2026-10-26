package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
// 推送内容：
//   - event: stats  —— 任务状态计数（每次任务状态变化时）
//   - event: jobs   —— 当前页任务列表的完整快照（节流后推送，保证客户端
//     无需再发请求就能刷新表格）
//
// 节流策略：最快每 1s 推一次快照，进度变化不到 1% 不触发。
// 面板断开后 context 取消，goroutine 退出，不泄漏。
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

	// 立即推一帧，客户端不用等第一个 tick
	stats, _ := s.st.Stats()
	if !writeEvent("stats", stats) {
		return
	}
	jobs, _, _ := s.st.ListJobs(r.URL.Query().Get("status"), 50, 0)
	if !writeEvent("jobs", jobs) {
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastSig := ""
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			stats, err := s.st.Stats()
			if err != nil {
				continue
			}
			// 变化信号：状态计数 + 运行中任务的进度
			sig := fmt.Sprintf("%d-%d-%d-%d", stats.JobsPending, stats.JobsRunning,
				stats.JobsDone, stats.JobsFailed)
			if run, _, err := s.st.ListJobs("running", 20, 0); err == nil {
				for _, j := range run {
					sig += fmt.Sprintf("-%d:%.2f", j.ID, j.Progress)
				}
			}
			if sig == lastSig {
				continue
			}
			lastSig = sig
			if !writeEvent("stats", stats) {
				return
			}
			jobs, _, err := s.st.ListJobs(r.URL.Query().Get("status"), 50, 0)
			if err != nil {
				continue
			}
			if !writeEvent("jobs", jobs) {
				return
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
