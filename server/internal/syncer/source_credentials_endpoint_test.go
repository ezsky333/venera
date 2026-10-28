package syncer_test

import (
	"errors"
	"testing"

	"github.com/venera-app/venera-nas/internal/model"
	"github.com/venera-app/venera-nas/internal/store"
	"github.com/venera-app/venera-nas/internal/syncer"
)

var errTestHook = errors.New("hook boom")

// TestBuildSourceCredentials 覆盖 GET /api/v1/sync/source-credentials 的数据源。
//
// 这个端点是"pica 账号没同步过来"的直接解药：客户端增量同步路径
// （游标已存在）不会走快照，只靠它补登录态。它此前只存在于一个
// 没进仓库的副本里，线上一直 404，于是 Cookie 永远补不上。
func TestBuildSourceCredentials(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sy := syncer.New(st)

	// 空库：应返回空结构而不是报错（客户端据此判断"本地已齐"）。
	creds, err := sy.BuildSourceCredentials()
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	if len(creds.Data) != 0 || len(creds.Cookies) != 0 {
		t.Fatalf("empty store returned data=%v cookies=%v", creds.Data, creds.Cookies)
	}

	// 写入 pica 的配置与 Cookie。
	if err := st.PutSourceData("picacg", map[string]any{
		"account": "u",
		"token":   "jwt",
	}); err != nil {
		t.Fatalf("put source data: %v", err)
	}
	if err := st.PutCookies("picacg", []model.CookieRecord{
		{Name: "ipb_member_id", Value: "5762139", Domain: ".e-hentai.org", Path: "/"},
	}); err != nil {
		t.Fatalf("put cookies: %v", err)
	}

	creds, err = sy.BuildSourceCredentials()
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	if got := creds.Data["picacg"]["account"]; got != "u" {
		t.Fatalf("picacg account = %v, want u", got)
	}
	if len(creds.Cookies["picacg"]) != 1 {
		t.Fatalf("picacg cookies = %v, want 1 entry", creds.Cookies["picacg"])
	}
	if creds.Cookies["picacg"][0].Value != "5762139" {
		t.Fatalf("cookie value = %q", creds.Cookies["picacg"][0].Value)
	}
}

// TestSourceDataOpTriggersCredentialsHook 确认 source.data / source.cookie
// 落库后会触发 OnSourceCredentials —— 这是"叫醒被推迟的下载任务"的入口。
func TestSourceDataOpTriggersCredentialsHook(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sy := syncer.New(st)
	var got []string
	sy.OnSourceCredentials = func(sourceKey string) error {
		got = append(got, sourceKey)
		return nil
	}

	_, err = sy.Push(syncer.PushRequest{
		ClientID:   "dev-1",
		DeviceName: "test",
		Lamport:    1,
		Ops: []syncer.PushOp{
			{Type: "source.data", Payload: map[string]any{
				"source": "picacg", "data": map[string]any{"account": "u"},
			}},
			{Type: "source.cookie", Payload: map[string]any{
				"source": "ehentai", "cookies": []any{
					map[string]any{"name": "ipb_member_id", "value": "1", "domain": ".e-hentai.org"},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	if len(got) != 2 || got[0] != "picacg" || got[1] != "ehentai" {
		t.Fatalf("hook calls = %v, want [picacg ehentai]", got)
	}
}

// TestCredentialsHookErrorDoesNotDropOp 确认回调出错不会让整批操作失败：
// 叫醒任务失败是运维问题，不该让"登录态已经写进库"这件事被回滚。
func TestCredentialsHookErrorDoesNotDropOp(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir+"/data", dir+"/library")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sy := syncer.New(st)
	sy.OnSourceCredentials = func(string) error {
		return errTestHook
	}

	resp, err := sy.Push(syncer.PushRequest{
		ClientID: "dev-1",
		Lamport:  1,
		Ops: []syncer.PushOp{
			{Type: "source.data", Payload: map[string]any{
				"source": "jm", "data": map[string]any{"baseUrl": "https://x"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if resp.Applied != 1 {
		t.Fatalf("applied = %d, want 1", resp.Applied)
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning about the hook failure")
	}
	// 数据必须已经落库
	data, err := st.GetSourceData("jm")
	if err != nil {
		t.Fatalf("get source data: %v", err)
	}
	if data["baseUrl"] != "https://x" {
		t.Fatalf("source data not persisted: %v", data)
	}
}
