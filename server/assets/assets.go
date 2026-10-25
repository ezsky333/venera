// Package assets 以 go:embed 方式打包 venera 的 JS 桥（init.js）。
//
// init.js 是 App 与漫画源之间的约定层：所有 Network/Convert/Html/UI API
// 都定义在这里，通过全局 sendMessage 回调到宿主。
// 服务端必须使用与 App 完全相同的一份，否则源的行为会漂移。
package assets

import _ "embed"

// InitJS 是 venera App 内置的 JS 库源码。
//
//go:embed init.js
var InitJS string
