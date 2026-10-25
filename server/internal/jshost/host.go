// Package jshost 在纯 Go 环境中复现 Venera App 的 JS 漫画源运行环境。
//
// App 侧的实现是 flutter_qjs(QuickJS) + assets/init.js（JS 桥）+ 一组
// sendMessage 回调（见 lib/foundation/js_engine.dart）。本包用 goja 替代
// QuickJS，并实现同一套 sendMessage 协议，使 venera-configs 的源脚本
// 可以原样运行。
//
// 并发模型（关键约束）：goja.Runtime 不是线程安全的。因此每个 Host 独占
// 一个 goroutine 执行所有 JS 代码，所有异步完成（HTTP 响应、定时器）都通过
// 事件队列投递回该 goroutine。
package jshost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/venera-app/venera-nas/internal/model"
)

// SourceDataStore 提供源的持久化读写，对应 App 的 <key>.data 文件与 cookie.db。
type SourceDataStore interface {
	GetSourceData(sourceKey string) (map[string]any, error)
	MergeSourceData(sourceKey string, patch map[string]any) error
	GetCookies(sourceKey string) ([]model.CookieRecord, error)
	PutCookies(sourceKey string, cookies []model.CookieRecord) error
}

// Options 控制单个 Host 的行为。
type Options struct {
	SourceKey  string
	InitJS     string
	SourceJS   string
	Store      SourceDataStore
	HTTPClient HTTPDoer
	Locale     string
	Platform   string
	AppVersion string
	Logger     func(level, title, content string)
	Limiter    RateLimiter
	// Timeout 是单次顶层 JS 调用的整体超时（含所有 await）。
	Timeout time.Duration
	// MaxResponseBytes 限制单次响应体大小，防止 OOM。
	MaxResponseBytes int64
}

// Host 是一个可执行漫画源方法的 JS 运行时。
type Host struct {
	vm     *goja.Runtime
	opts   Options
	events chan func()
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool

	source  goja.Value
	typ     string
	helpers *jsHelpers

	rng   *rand.Rand
	rngMu sync.Mutex
}

// New 创建 Host 并加载 init.js 与源脚本。
// 返回后必须调用 RunLoop 驱动事件循环，且所有 JS 调用都应在该 goroutine 上进行。
func New(opts Options) (*Host, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 64 << 20
	}
	if opts.Locale == "" {
		opts.Locale = "zh_CN"
	}
	if opts.Platform == "" {
		opts.Platform = "linux"
	}
	if opts.AppVersion == "" {
		opts.AppVersion = "1.6.3"
	}
	if opts.Logger == nil {
		opts.Logger = func(level, title, content string) {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &Host{
		vm:      goja.New(),
		opts:    opts,
		events:  make(chan func(), 512),
		ctx:     ctx,
		cancel:  cancel,
		helpers: &jsHelpers{},
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	if err := h.install(); err != nil {
		cancel()
		return nil, err
	}
	if err := h.load(); err != nil {
		cancel()
		return nil, err
	}
	return h, nil
}

func (h *Host) install() error {
	vm := h.vm
	if err := vm.Set("sendMessage", func(call goja.FunctionCall) goja.Value {
		return h.dispatch(call.Argument(0))
	}); err != nil {
		return err
	}
	if err := vm.Set("appVersion", h.opts.AppVersion); err != nil {
		return err
	}
	// init.js 里的 setTimeout 依赖 sendMessage({method:'delay'})，无需额外实现。
	// 但少数源可能直接用原生 setTimeout，这里补一个。
	if err := vm.Set("__nativeSetTimeout", func(call goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			return goja.Undefined()
		}
		ms := call.Argument(1).ToInteger()
		if ms < 0 {
			ms = 0
		}
		time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
			h.post(func() { _, _ = fn(goja.Undefined()) })
		})
		return goja.Undefined()
	}); err != nil {
		return err
	}
	return nil
}

func (h *Host) load() error {
	vm := h.vm
	if _, err := vm.RunString(h.opts.InitJS); err != nil {
		return fmt.Errorf("run init.js: %w", err)
	}
	if _, err := vm.RunString(h.opts.SourceJS); err != nil {
		return fmt.Errorf("run source js: %w", err)
	}
	cls, err := detectClassName(h.opts.SourceJS)
	if err != nil {
		return err
	}
	h.typ = cls
	if _, err := vm.RunString("globalThis.__source = new " + cls + "();"); err != nil {
		return fmt.Errorf("instantiate %s: %w", cls, err)
	}
	src := vm.Get("__source")
	if src == nil || goja.IsUndefined(src) || goja.IsNull(src) {
		return fmt.Errorf("source instance is nil")
	}
	h.source = src
	return nil
}

// detectClassName 复现 App 解析器：取第一行以 "class " 开头且 extends ComicSource 的声明。
func detectClassName(js string) (string, error) {
	js = strings.ReplaceAll(js, "\r\n", "\n")
	for _, line := range strings.Split(js, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "class ") || !strings.Contains(t, "extends ComicSource") {
			continue
		}
		name := strings.SplitN(t, "extends ComicSource", 2)[0]
		name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "class "))
		name = strings.TrimSpace(strings.TrimSuffix(name, "{"))
		if name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no class extending ComicSource found")
}

// post 把回调投递到 Host 所属的事件循环。
func (h *Host) post(fn func()) {
	select {
	case h.events <- fn:
	case <-h.ctx.Done():
	}
}

// RunLoop 在调用方 goroutine 上驱动事件循环，直到 Close。
func (h *Host) RunLoop() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case fn := <-h.events:
			fn()
		}
	}
}

// Close 释放资源。
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	h.cancel()
}

func (h *Host) logf(level, title, msg string) { h.opts.Logger(level, title, msg) }

// dispatch 实现 App 的 _messageReceiver 协议。
func (h *Host) dispatch(arg goja.Value) goja.Value {
	vm := h.vm
	var msg map[string]any
	if err := vm.ExportTo(arg, &msg); err != nil {
		return goja.Null()
	}
	method, _ := msg["method"].(string)

	switch method {
	case "log":
		level, _ := msg["level"].(string)
		title, _ := msg["title"].(string)
		h.logf(level, title, fmt.Sprint(msg["content"]))
		return goja.Undefined()

	case "http":
		return h.jsHTTP(msg)

	case "convert":
		return h.convertMap(msg)

	case "html":
		return h.jsHTML(msg)

	case "cookie":
		return h.jsCookie(msg)

	case "delay":
		ms := toInt(msg["time"])
		if ms < 0 {
			ms = 0
		}
		return vm.ToValue(h.promiseAfter(time.Duration(ms) * time.Millisecond))

	case "load_data":
		return h.loadData(fmt.Sprint(msg["key"]), fmt.Sprint(msg["data_key"]))

	case "save_data":
		h.saveData(fmt.Sprint(msg["key"]), fmt.Sprint(msg["data_key"]), msg["data"])
		return goja.Undefined()

	case "delete_data":
		h.deleteData(fmt.Sprint(msg["key"]), fmt.Sprint(msg["data_key"]))
		return goja.Undefined()

	case "load_setting":
		return h.loadSetting(fmt.Sprint(msg["key"]), fmt.Sprint(msg["setting_key"]))

	case "isLogged":
		return vm.ToValue(h.isLogged())

	case "random":
		return h.jsRandom(msg["min"], msg["max"], fmt.Sprint(msg["type"]))

	case "uuid":
		return vm.ToValue(randomUUID())

	case "getLocale":
		return vm.ToValue(h.opts.Locale)

	case "getPlatform":
		return vm.ToValue(h.opts.Platform)

	case "UI":
		return h.jsUI(msg)

	case "compute":
		fnStr, _ := msg["function"].(string)
		args, _ := msg["args"].([]any)
		return h.jsCompute(fnStr, args)

	case "image":
		return h.jsImage(msg)

	case "setClipboard", "getClipboard":
		return goja.Undefined()
	}
	return goja.Null()
}

func (h *Host) promiseAfter(d time.Duration) *goja.Promise {
	p, resolve, _ := h.vm.NewPromise()
	t := time.AfterFunc(d, func() {
		h.post(func() { resolve(goja.Undefined()) })
	})
	go func() {
		<-h.ctx.Done()
		t.Stop()
	}()
	return p
}

func (h *Host) jsRandom(minV, maxV any, typ string) goja.Value {
	mn := float64(toInt(minV))
	mx := float64(toInt(maxV))
	if mx == 0 && mn == 0 {
		mx = 1
	}
	delta := mx - mn
	if delta <= 0 {
		delta = 1
	}
	h.rngMu.Lock()
	r := h.rng.Float64()
	h.rngMu.Unlock()
	if typ == "double" {
		return h.vm.ToValue(mn + delta*r)
	}
	return h.vm.ToValue(int64(mn + delta*r))
}

// ---------- 源数据读写 ----------

func (h *Host) loadData(key, dataKey string) goja.Value {
	if h.opts.Store == nil {
		return goja.Null()
	}
	data, err := h.opts.Store.GetSourceData(key)
	if err != nil {
		return goja.Null()
	}
	v, ok := data[dataKey]
	if !ok || v == nil {
		return goja.Null()
	}
	return h.valueFrom(v)
}

// loadSetting 读取源的自定义设置（源脚本里 settings 声明的那批键）。
//
// 与 App 的 js_engine.dart 保持同一套回退顺序：
//
//	1. 用户保存过的值（来自 source_data.data["settings"]）
//	2. 源脚本里该键声明的 default
//
// 服务端此前一律返回 null，源脚本把 `${loadSetting('base_url')}/comics/x`
// 拼成 "null/comics/x"（picacg 的经典报错）。只做第 1 步也不够：
// 用户没改过的设置压根不在 source_data 里，必须回落到脚本的 default。
func (h *Host) loadSetting(key, settingKey string) goja.Value {
	if settingKey == "" {
		return goja.Undefined()
	}

	// 1) 用户保存的值
	if h.opts.Store != nil {
		if data, err := h.opts.Store.GetSourceData(key); err == nil {
			if settings, ok := data["settings"]; ok && settings != nil {
				if v, ok := lookupSetting(settings, settingKey); ok && v != nil {
					return h.valueFrom(v)
				}
			}
		}
	}

	// 2) 源脚本声明的默认值
	if def, ok := h.settingDefault(settingKey); ok {
		return def
	}
	return goja.Undefined()
}

// settingDefault 从已实例化的源对象上读 settings[key].default。
func (h *Host) settingDefault(settingKey string) (goja.Value, bool) {
	if h.vm == nil {
		return nil, false
	}
	src := h.source
	if src == nil || goja.IsUndefined(src) || goja.IsNull(src) {
		return nil, false
	}
	obj, ok := src.(*goja.Object)
	if !ok {
		return nil, false
	}
	settings := obj.Get("settings")
	if settings == nil || goja.IsUndefined(settings) || goja.IsNull(settings) {
		return nil, false
	}
	sObj, ok := settings.(*goja.Object)
	if !ok {
		return nil, false
	}
	// 支持 "a.b.c" 形式的嵌套键
	var cur goja.Value = sObj
	for _, part := range strings.Split(settingKey, ".") {
		o, ok := cur.(*goja.Object)
		if !ok {
			return nil, false
		}
		cur = o.Get(part)
		if cur == nil || goja.IsUndefined(cur) || goja.IsNull(cur) {
			return nil, false
		}
	}
	spec, ok := cur.(*goja.Object)
	if !ok {
		return nil, false
	}
	def := spec.Get("default")
	if def == nil || goja.IsUndefined(def) || goja.IsNull(def) {
		return nil, false
	}
	return def, true
}

// lookupSetting 支持 "a.b.c" 形式的嵌套键，也兼容扁平键。
func lookupSetting(node any, path string) (any, bool) {
	if m, ok := node.(map[string]any); ok {
		if v, ok := m[path]; ok {
			return v, true
		}
	}
	cur := node
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func (h *Host) saveData(key, dataKey string, data any) {	if h.opts.Store == nil || dataKey == "setting" {
		return
	}
	val := exportToPlain(data)
	if err := h.opts.Store.MergeSourceData(key, map[string]any{dataKey: val}); err != nil {
		h.logf("error", "saveData", err.Error())
	}
}

func (h *Host) deleteData(key, dataKey string) {
	if h.opts.Store == nil {
		return
	}
	if err := h.opts.Store.MergeSourceData(key, map[string]any{dataKey: nil}); err != nil {
		h.logf("error", "deleteData", err.Error())
	}
}

func (h *Host) isLogged() bool {
	if h.opts.Store == nil {
		return false
	}
	data, err := h.opts.Store.GetSourceData(h.opts.SourceKey)
	if err != nil {
		return false
	}
	v, ok := data["account"]
	return ok && v != nil
}

// valueFrom 把 Go 值转成 goja 值（走 JSON，避免手写类型映射遗漏）。
func (h *Host) valueFrom(v any) goja.Value {
	b, err := json.Marshal(v)
	if err != nil {
		return goja.Null()
	}
	out, err := h.vm.RunString("(" + string(b) + ")")
	if err != nil {
		return goja.Null()
	}
	return out
}

// exportToPlain 把 goja 值递归转成可 JSON 序列化的 Go 值。
func exportToPlain(v any) any {
	switch x := v.(type) {
	case goja.Value:
		if goja.IsUndefined(x) || goja.IsNull(x) {
			return nil
		}
		return exportToPlain(x.Export())
	case goja.ArrayBuffer:
		return base64.StdEncoding.EncodeToString(x.Bytes())
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = exportToPlain(vv)
		}
		return out
	case map[string]goja.Value:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = exportToPlain(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = exportToPlain(vv)
		}
		return out
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	}
	return v
}

// ---------- 顶层调用 ----------

// Call 在事件循环线程上执行一个返回 Promise 的源方法，并等待其完成。
// 必须在 RunLoop 所在的 goroutine 上调用。
func (h *Host) Call(expr string) (goja.Value, error) {
	timeout := h.opts.Timeout
	deadline := time.Now().Add(timeout)

	v, err := h.vm.RunString(expr)
	if err != nil {
		return nil, err
	}
	p, ok := v.Export().(*goja.Promise)
	if !ok {
		// 源方法可能是同步的
		return v, nil
	}

	for p.State() == goja.PromiseStatePending {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("js call timed out after %s", timeout)
		}
		select {
		case fn := <-h.events:
			fn()
		case <-h.ctx.Done():
			return nil, fmt.Errorf("host closed")
		case <-time.After(20 * time.Millisecond):
			// 让出，等待异步完成投递回事件队列
		}
	}
	if p.State() == goja.PromiseStateRejected {
		return nil, fmt.Errorf("js rejected: %v", p.Result())
	}
	return p.Result(), nil
}

// Type 返回源类名。
func (h *Host) Type() string { return h.typ }
