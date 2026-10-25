package jshost

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
	"golang.org/x/net/html"
)

// Document 包装一棵 HTML 树，向 JS 暴露与 App 相同的 DOM 子集。
//
// App 侧（lib/foundation/js_engine.dart 的 DocumentWrapper）用 dart:html 的
// 解析器，并把元素存进数组用整数 key 引用。这里用 x/net/html 复刻同样的模型：
// 每个 node 分配一个整数 id，JS 侧只持有 id。
type Document struct {
	root  *html.Node
	nodes []*html.Node // id -> node
}

func newDocument(src string) (*Document, error) {
	root, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return nil, err
	}
	return &Document{root: root}, nil
}

// intern 为节点分配（或复用）一个 id。
func (d *Document) intern(n *html.Node) int {
	if n == nil {
		return -1
	}
	// 线性查找足够快：单次解析通常 < 2000 节点，且只在查询时调用
	for i, e := range d.nodes {
		if e == n {
			return i
		}
	}
	d.nodes = append(d.nodes, n)
	return len(d.nodes) - 1
}

func (d *Document) get(id int) *html.Node {
	if id < 0 || id >= len(d.nodes) {
		return nil
	}
	return d.nodes[id]
}

// jsHTML 实现 new HtmlDocument(...) 与后续的所有 DOM 操作。
func (h *Host) jsHTML(msg map[string]any) goja.Value {
	vm := h.vm
	fn, _ := msg["function"].(string)

	switch fn {
	case "parse":
		raw, _ := msg["data"].(string)
		doc, err := newDocument(raw)
		if err != nil {
			h.opts.Logger("warning", "HTML", "parse failed: "+err.Error())
			return goja.Null()
		}
		key := toInt(msg["key"])
		h.helpers.docs.Store(key, doc)
		return goja.Undefined()

	case "dispose":
		key := toInt(msg["key"])
		h.helpers.docs.Delete(key)
		return goja.Undefined()
	}

	docKey := toInt(msg["key"])
	if fn == "getElementById" {
		doc := h.getDoc(docKey)
		if doc == nil {
			return goja.Null()
		}
		return h.nullableID(doc.intern(findByID(doc.root, fmtStr(msg["id"]))))
	}

	doc := h.getDoc(toInt(msg["doc"]))
	if doc == nil {
		// 部分调用把文档 key 放在 key 上
		doc = h.getDoc(docKey)
	}
	if doc == nil {
		return goja.Null()
	}

	nodeKey := toInt(msg["key"])
	node := doc.get(int(nodeKey))
	if node == nil {
		return goja.Null()
	}

	switch fn {
	case "querySelector":
		el := querySelector(node, fmtStr(msg["query"]))
		return h.nullableID(doc.intern(el))

	case "querySelectorAll", "dom_querySelectorAll":
		els := querySelectorAll(node, fmtStr(msg["query"]))
		return h.idArray(els, doc)

	case "dom_querySelector":
		el := querySelector(node, fmtStr(msg["query"]))
		return h.nullableID(doc.intern(el))

	case "getText":
		return vm.ToValue(textOf(node))

	case "getAttributes":
		obj := vm.NewObject()
		for _, a := range node.Attr {
			_ = obj.Set(a.Key, a.Val)
		}
		return obj

	case "getChildren":
		var els []*html.Node
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				els = append(els, c)
			}
		}
		return h.idArray(els, doc)

	case "getNodes":
		var els []*html.Node
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			els = append(els, c)
		}
		return h.idArray(els, doc)

	case "getInnerHTML":
		var b strings.Builder
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			_ = html.Render(&b, c)
		}
		return vm.ToValue(b.String())

	case "getParent":
		return h.nullableID(doc.intern(node.Parent))

	case "getPreviousSibling":
		return h.nullableID(doc.intern(prevElement(node)))

	case "getNextSibling":
		return h.nullableID(doc.intern(nextElement(node)))

	case "getClassNames":
		cls := attrOf(node, "class")
		if cls == "" {
			return vm.NewArray()
		}
		arr := vm.NewArray()
		for i, c := range strings.Fields(cls) {
			arr.Set(fmt.Sprint(i), vm.ToValue(c))
		}
		return arr

	case "getId":
		id := attrOf(node, "id")
		if id == "" {
			return goja.Null()
		}
		return vm.ToValue(id)

	case "getLocalName":
		return vm.ToValue(node.Data)

	case "node_text":
		return vm.ToValue(textOf(node))

	case "node_type":
		return vm.ToValue(nodeTypeName(node))

	case "node_toElement":
		if node.Type == html.ElementNode {
			return vm.ToValue(doc.intern(node))
		}
		return goja.Null()
	}
	return goja.Null()
}

func fmtStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (h *Host) nullableID(id int) goja.Value {
	if id < 0 {
		return goja.Null()
	}
	return h.vm.ToValue(id)
}

func (h *Host) idArray(nodes []*html.Node, doc *Document) goja.Value {
	arr := h.vm.NewArray()
	for i, n := range nodes {
		arr.Set(fmt.Sprint(i), h.vm.ToValue(doc.intern(n)))
	}
	return arr
}

func (h *Host) getDoc(key int64) *Document {
	v, ok := h.helpers.docs.Load(key)
	if !ok {
		return nil
	}
	return v.(*Document)
}

func nodeTypeName(n *html.Node) string {
	switch n.Type {
	case html.ElementNode:
		return "element"
	case html.TextNode:
		return "text"
	case html.CommentNode:
		return "comment"
	case html.DocumentNode:
		return "document"
	}
	return "unknown"
}

// textOf 递归收集文本，等价于 DOM 的 textContent。
//
// 要点：
//   - DocumentNode（html.Parse 的根）需要向下进入子节点；
//   - 跳过 head / script / style / noscript，贴近 App 的 innerText 直觉。
func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		switch x.Type {
		case html.TextNode:
			b.WriteString(x.Data)
		case html.DocumentNode:
			for c := x.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		case html.ElementNode:
			switch x.Data {
			case "script", "style", "noscript", "template", "head":
				return
			}
			for c := x.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, cls string) bool {
	for _, c := range strings.Fields(attrOf(n, "class")) {
		if c == cls {
			return true
		}
	}
	return false
}

func findByID(root *html.Node, id string) *html.Node {
	if id == "" {
		return nil
	}
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && attrOf(n, "id") == id {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

func prevElement(n *html.Node) *html.Node {
	for p := n.PrevSibling; p != nil; p = p.PrevSibling {
		if p.Type == html.ElementNode {
			return p
		}
	}
	return nil
}

func nextElement(n *html.Node) *html.Node {
	for p := n.NextSibling; p != nil; p = p.NextSibling {
		if p.Type == html.ElementNode {
			return p
		}
	}
	return nil
}

// ---------- CSS 选择器 ----------

// querySelector 返回第一个匹配元素。
func querySelector(root *html.Node, sel string) *html.Node {
	groups := splitTopLevel(sel, ',')
	for _, g := range groups {
		chain := parseChain(g)
		if len(chain) == 0 {
			continue
		}
		if n := matchChain(root, chain, true); n != nil {
			return n
		}
	}
	return nil
}

// querySelectorAll 返回所有匹配元素（文档顺序、去重）。
func querySelectorAll(root *html.Node, sel string) []*html.Node {
	groups := splitTopLevel(sel, ',')
	seen := map[*html.Node]bool{}
	var out []*html.Node

	var all []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			all = append(all, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	for _, n := range all {
		for _, g := range groups {
			chain := parseChain(g)
			if len(chain) == 0 {
				continue
			}
			if matchChainReverse(n, chain) {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
				break
			}
		}
	}
	return out
}

// selectorStep 是选择器链中的一环，如 div.foo[data-x] > span
type selectorStep struct {
	tag      string
	id       string
	classes  []string
	attrs    map[string]string
	attrHas  []string
	combin   byte // ' ' 后代, '>' 子, '+' 相邻, '~' 兄弟
	notStep  *selectorStep
	pseudo   string
}

func splitTopLevel(sel string, sep rune) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for _, r := range sel {
		switch r {
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
			}
		}
		if r == sep && depth == 0 {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// parseChain 把 "a > b c" 拆成带 combinator 的步骤序列。
func parseChain(sel string) []selectorStep {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil
	}
	var steps []selectorStep
	var cur strings.Builder
	pendingCombin := byte(' ')

	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" {
			return
		}
		st := parseStep(s)
		st.combin = pendingCombin
		steps = append(steps, st)
		pendingCombin = ' '
	}

	for i := 0; i < len(sel); i++ {
		c := sel[i]
		switch c {
		case '>', '+', '~':
			flush()
			pendingCombin = c
		case ' ':
			if cur.Len() > 0 {
				flush()
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return steps
}

func parseStep(s string) selectorStep {
	st := selectorStep{attrs: map[string]string{}}
	// 处理 :not(...)
	if idx := strings.Index(s, ":not("); idx >= 0 {
		end := strings.LastIndex(s, ")")
		if end > idx {
			inner := s[idx+5 : end]
			sub := parseStep(inner)
			st.notStep = &sub
			s = s[:idx] + s[end+1:]
		}
	}
	// 处理其他伪类（:first-child 等）——仅记录，后续按需处理
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		st.pseudo = s[idx+1:]
		if p := strings.IndexByte(st.pseudo, '('); p >= 0 {
			st.pseudo = st.pseudo[:p]
		}
		s = s[:idx]
	}

	i := 0
	// tag
	for i < len(s) && s[i] != '.' && s[i] != '#' && s[i] != '[' {
		i++
	}
	st.tag = strings.ToLower(s[:i])

	for i < len(s) {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && s[j] != '.' && s[j] != '#' && s[j] != '[' {
				j++
			}
			st.classes = append(st.classes, s[i+1:j])
			i = j
		case '#':
			j := i + 1
			for j < len(s) && s[j] != '.' && s[j] != '#' && s[j] != '[' {
				j++
			}
			st.id = s[i+1 : j]
			i = j
		case '[':
			j := i + 1
			depth := 1
			for j < len(s) && depth > 0 {
				if s[j] == '[' {
					depth++
				}
				if s[j] == ']' {
					depth--
					if depth == 0 {
						break
					}
				}
				j++
			}
			inner := s[i+1 : j]
			i = j + 1
			// 支持 [k], [k=v], [k="v"], [k^=v], [k*=v]
			if k, v, ok := strings.Cut(inner, "="); ok {
				k = strings.TrimSpace(k)
				op := "="
				if strings.HasSuffix(k, "^") || strings.HasSuffix(k, "*") || strings.HasSuffix(k, "$") ||
					strings.HasSuffix(k, "|") || strings.HasSuffix(k, "~") {
					op = string(k[len(k)-1]) + "="
					k = k[:len(k)-1]
				}
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				st.attrs[k+"\x00"+op] = v
			} else {
				st.attrHas = append(st.attrHas, strings.TrimSpace(inner))
			}
		default:
			i++
		}
	}
	return st
}

func stepMatches(n *html.Node, st selectorStep) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if st.tag != "" && st.tag != "*" && strings.ToLower(n.Data) != st.tag {
		return false
	}
	if st.id != "" && attrOf(n, "id") != st.id {
		return false
	}
	for _, c := range st.classes {
		if !hasClass(n, c) {
			return false
		}
	}
	for _, a := range st.attrHas {
		if attrOf(n, a) == "" {
			if !hasAttr(n, a) {
				return false
			}
		}
	}
	for k, want := range st.attrs {
		name, op, _ := strings.Cut(k, "\x00")
		got := attrOf(n, name)
		switch op {
		case "=":
			if got != want {
				return false
			}
		case "^=":
			if !strings.HasPrefix(got, want) {
				return false
			}
		case "*=":
			if !strings.Contains(got, want) {
				return false
			}
		case "$=":
			if !strings.HasSuffix(got, want) {
				return false
			}
		case "~=":
			found := false
			for _, f := range strings.Fields(got) {
				if f == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	if st.notStep != nil && stepMatches(n, *st.notStep) {
		return false
	}
	switch st.pseudo {
	case "first-child":
		if prevElement(n) != nil {
			return false
		}
	case "last-child":
		if nextElement(n) != nil {
			return false
		}
	case "empty":
		if n.FirstChild != nil {
			return false
		}
	}
	return true
}

func hasAttr(n *html.Node, k string) bool {
	for _, a := range n.Attr {
		if a.Key == k {
			return true
		}
	}
	return false
}

// matchChainReverse 从候选节点向上回溯匹配。
func matchChainReverse(n *html.Node, chain []selectorStep) bool {
	if len(chain) == 0 {
		return false
	}
	if !stepMatches(n, chain[len(chain)-1]) {
		return false
	}
	return matchUp(n, chain, len(chain)-1)
}

func matchUp(n *html.Node, chain []selectorStep, idx int) bool {
	if idx == 0 {
		return true
	}
	cur := chain[idx]
	switch cur.combin {
	case '>':
		p := n.Parent
		for p != nil && p.Type != html.ElementNode {
			p = p.Parent
		}
		if p == nil {
			return false
		}
		if !stepMatches(p, chain[idx-1]) {
			return false
		}
		return matchUp(p, chain, idx-1)
	case '+':
		p := prevElement(n)
		if p == nil || !stepMatches(p, chain[idx-1]) {
			return false
		}
		return matchUp(p, chain, idx-1)
	case '~':
		for p := prevElement(n); p != nil; p = prevElement(p) {
			if stepMatches(p, chain[idx-1]) && matchUp(p, chain, idx-1) {
				return true
			}
		}
		return false
	default: // 后代
		for p := n.Parent; p != nil; p = p.Parent {
			if p.Type != html.ElementNode {
				continue
			}
			if stepMatches(p, chain[idx-1]) && matchUp(p, chain, idx-1) {
				return true
			}
		}
		return false
	}
}

// matchChain 从 root 开始向后查找（querySelector 用）。
func matchChain(root *html.Node, chain []selectorStep, fromRoot bool) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && matchChainReverse(n, chain) {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}
