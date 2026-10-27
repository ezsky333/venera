package jobs

import (
	"testing"
	"time"
)

// TestHubReportsProgressAndSpeed 验证实时进度与速度计算。
func TestHubReportsProgressAndSpeed(t *testing.T) {
	h := NewHub()
	defer h.Close()

	h.Report(Live{JobID: 1, Bytes: 0, Progress: 0.1, Status: "running"})
	got, ok := h.Get(1)
	if !ok || got.Progress != 0.1 {
		t.Fatalf("expected progress 0.1, got %+v ok=%v", got, ok)
	}

	// 模拟 100KB / 约 100ms
	time.Sleep(120 * time.Millisecond)
	h.Report(Live{JobID: 1, Bytes: 100 * 1024, Progress: 0.2})
	got, _ = h.Get(1)
	if got.Bytes != 100*1024 {
		t.Fatalf("bytes not tracked: %d", got.Bytes)
	}
	if got.SpeedBps <= 0 {
		t.Fatalf("speed should be positive, got %f", got.SpeedBps)
	}
	// 约 100KB/0.12s ≈ 830KB/s，允许较宽区间（CI 调度抖动）
	if got.SpeedBps < 50*1024 || got.SpeedBps > 5*1024*1024 {
		t.Fatalf("speed out of plausible range: %f B/s", got.SpeedBps)
	}
}

// TestHubClearRemovesTask 验证任务结束后实时记录被清理，
// 否则内存会随任务数无限增长。
func TestHubClearRemovesTask(t *testing.T) {
	h := NewHub()
	defer h.Close()

	h.Report(Live{JobID: 7, Progress: 0.5})
	if _, ok := h.Get(7); !ok {
		t.Fatal("task should be present")
	}
	h.Clear(7)
	if _, ok := h.Get(7); ok {
		t.Fatal("task should be gone after Clear")
	}
	if len(h.Snapshot()) != 0 {
		t.Fatalf("snapshot should be empty, got %d", len(h.Snapshot()))
	}
}

// TestHubSubscribeGetsFrames 验证订阅者收到合并后的实时帧。
func TestHubSubscribeGetsFrames(t *testing.T) {
	h := NewHub()
	defer h.Close()

	ch, cancel := h.Subscribe()
	defer cancel()

	// 首帧立刻到达
	select {
	case frame := <-ch:
		if len(frame) != 0 {
			t.Fatalf("initial frame should be empty, got %d entries", len(frame))
		}
	case <-time.After(time.Second):
		t.Fatal("no initial frame")
	}

	h.Report(Live{JobID: 3, Progress: 0.42})
	select {
	case frame := <-ch:
		if len(frame) != 1 || frame[0].JobID != 3 {
			t.Fatalf("unexpected frame: %+v", frame)
		}
		if frame[0].Progress != 0.42 {
			t.Fatalf("progress not propagated: %v", frame[0].Progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no progress frame within 2s")
	}
}

// TestHubCoalescesBurstUpdates 验证高频更新被合并：
// 一秒内上千次 Report 不应产生上千帧（否则浏览器会被淹没）。
func TestHubCoalescesBurstUpdates(t *testing.T) {
	h := NewHub()
	defer h.Close()

	ch, cancel := h.Subscribe()
	defer cancel()
	<-ch // 丢弃首帧

	for i := 0; i < 2000; i++ {
		h.Report(Live{JobID: 9, Progress: float64(i) / 2000.0})
	}

	// 收集 ~1 秒内的帧数
	count := 0
	deadline := time.After(time.Second)
	for {
		select {
		case <-ch:
			count++
		case <-deadline:
			// 200ms 合并间隔 → 1 秒最多约 5 帧
			if count == 0 {
				t.Fatal("expected at least one coalesced frame")
			}
			if count > 12 {
				t.Fatalf("frames not coalesced: %d frames in 1s", count)
			}
			return
		}
	}
}

// TestHubStateVersionBumpsOnStatusChange 验证状态跃变能被 SSE 感知。
func TestHubStateVersionBumpsOnStatusChange(t *testing.T) {
	h := NewHub()
	defer h.Close()

	v0 := h.StateVersion()
	h.SetStatus(1, "done", "complete")
	if h.StateVersion() == v0 {
		t.Fatal("state version should increase on status change")
	}
}

// TestHubSnapshotIsCopy 验证快照是副本，外部修改不会污染内部状态。
func TestHubSnapshotIsCopy(t *testing.T) {
	h := NewHub()
	defer h.Close()

	h.Report(Live{JobID: 2, Progress: 0.5})
	snap := h.Snapshot()
	snap[0].Progress = 0.99

	got, _ := h.Get(2)
	if got.Progress != 0.5 {
		t.Fatalf("snapshot mutation leaked into hub: %v", got.Progress)
	}
}
