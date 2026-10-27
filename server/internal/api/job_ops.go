package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/venera-app/venera-nas/internal/jobs"
	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
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
//
//	POST /api/v1/jobs/{id}/pause | resume | top | untop | cancel | retry | delete
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
	case "delete":
		ok, err = s.st.DeleteJob(id)
		if errors.Is(err, store.ErrJobRunning) {
			// 运行中的任务不能删：worker 靠 jobs 行里的 message 判断
			// 该不该停，行没了它就再也停不下来（会继续下完整本）。
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "任务正在下载，请先暂停再删除",
			})
			return
		}
		if err == nil && ok && s.hub != nil {
			// 清掉内存里的实时状态，否则面板会继续显示这一行的进度
			s.hub.Clear(id)
			s.hub.BumpState()
		}
		msg = "deleted"
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
//
//	GET /api/v1/jobs/events
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

// handleComicJobs 返回**某一本漫画**的全部任务（含实时进度）。
//
//	GET /api/v1/jobs/by-comic?source=&comicId=
//
// 用途：详情页的 NAS 存储面板要在同一个弹窗里同时展示
//   - 已缓存的章节（来自 library 接口）
//   - 正在下载 / 排队中 / 失败的章节任务（来自这里）
//
// 与 /api/v1/jobs/events 的分工：SSE 负责"变化时推送"，
// 这个接口负责"打开弹窗时拿到初始快照"，两者组合即可实时。
//
// 返回的每项都合并了内存里的实时状态（进度/页数/速度），
// 因此客户端不用再逐个查 /jobs/{id}。
func (s *Server) handleComicJobs(w http.ResponseWriter, r *http.Request) {
	srcKey := r.URL.Query().Get("source")
	comicID := r.URL.Query().Get("comicId")
	if srcKey == "" || comicID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "source and comicId are required",
		})
		return
	}

	out := []map[string]any{}

	// 数据库里的任务（可能有同一本的多条历史记录）
	rows, err := s.st.DB().Query(`
		SELECT id, status, progress, message, COALESCE(error,''), COALESCE(origin,''),
		       COALESCE(title,''), COALESCE(chapters,''), created_at
		FROM jobs WHERE source = ? AND comic_id = ?
		ORDER BY id DESC LIMIT 20`, srcKey, comicID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id        int64
			status    string
			progress  float64
			message   string
			errMsg    string
			origin    string
			title     string
			chapters  string
			createdAt int64
		)
		if err := rows.Scan(&id, &status, &progress, &message, &errMsg,
			&origin, &title, &chapters, &createdAt); err != nil {
			continue
		}
		item := map[string]any{
			"id": id, "status": status, "progress": progress,
			"message": message, "error": errMsg, "origin": origin,
			"title": title, "createdAt": createdAt,
		}
		// 合并实时状态：数据库是 5 秒检查点，内存里才是最新值
		if s.hub != nil {
			if l, ok := s.hub.Get(id); ok {
				item["live"] = l
				item["progress"] = l.Progress
				item["status"] = l.Status
			}
		}
		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source": srcKey, "comicId": comicID, "jobs": out,
	})
}

// handleJobsBulk 批量操作：暂停全部 pending / 重试全部 failed / 清理终态任务。
//
//	POST /api/v1/jobs/bulk
//	body: {"action": "pause-all"|"resume-all"|"retry-failed"|"delete-finished",
//	       "statuses": ["done","failed","cancelled"]}   // 仅 delete-finished 使用
func (s *Server) handleJobsBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string   `json:"action"`
		Statuses []string `json:"statuses"`
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
	case "delete-finished":
		n, err := s.st.DeleteFinishedJobs(req.Statuses)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// 这些任务已不在库里，清掉残留的内存实时状态，免得面板显示幽灵行
		if s.hub != nil {
			s.hub.BumpState()
		}
		s.log.Info("jobs bulk delete", "removed", n)
		writeJSON(w, http.StatusOK, map[string]any{"affected": n})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown action: " + req.Action})
	}
}
