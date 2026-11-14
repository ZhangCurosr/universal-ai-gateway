package config

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 网页端管理池与服务（T72，PE-03、PE-04）：按名称增删改配置文件中 pools / models 序列的一项，
// 只改动该项的文本行，其他内容（注释、空行、其他项）原样不变。

// ListItem 是序列中的一项：名称与它在文件中的原文（去掉 "- " 与公共缩进后的映射文本）。
type ListItem struct {
	Name string
	Text string
	from int // 第一行（"- " 所在行，从 0 开始）
	to   int // 最后一行（含）
}

type seqInfo struct {
	lines    []string
	keyLine  int // "pools:" 所在行；-1 = 没有这个键
	flow     bool
	empty    bool // 值为空或 []
	indent   int  // "- " 的列（从 0 开始）
	items    []ListItem
	boundary int // 序列之后第一行（下一个顶层键或文件末尾）
}

func splitLines(data []byte) []string {
	s := string(data)
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

func analyzeSeq(data []byte, key string) (*seqInfo, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}
	si := &seqInfo{lines: splitLines(data), keyLine: -1, indent: 2}
	si.boundary = len(si.lines)
	if len(doc.Content) == 0 {
		return si, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config: the top level of the file is not a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != key {
			continue
		}
		si.keyLine = k.Line - 1
		if i+2 < len(root.Content) {
			si.boundary = root.Content[i+2].Line - 1
			// 下一个顶层键之前紧挨着的注释属于它
			for si.boundary-1 > si.keyLine && isBlankOrComment(si.lines[si.boundary-1]) && strings.TrimLeft(si.lines[si.boundary-1], " \t") == si.lines[si.boundary-1] {
				si.boundary--
			}
		}
		switch {
		case v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || v.Value == ""):
			si.empty = true
		case v.Kind != yaml.SequenceNode:
			return nil, fmt.Errorf("config: %s is not a list", key)
		case v.Style&yaml.FlowStyle != 0:
			if len(v.Content) > 0 {
				return nil, fmt.Errorf("config: %s is written in flow style ([...]); edit it by hand", key)
			}
			si.flow, si.empty = true, true
		default:
			for j, item := range v.Content {
				if item.Kind != yaml.MappingNode {
					return nil, fmt.Errorf("config: %s[%d] is not a mapping; edit it by hand", key, j)
				}
				from := item.Line - 1
				if j == 0 {
					si.indent = item.Column - 3
					if si.indent < 0 {
						si.indent = 0
					}
				}
				name := ""
				if _, nv := lookup(item, "name"); nv != nil {
					name = nv.Value
				}
				si.items = append(si.items, ListItem{Name: name, from: from})
			}
			for j := range si.items {
				end := si.boundary - 1
				if j+1 < len(si.items) {
					end = si.items[j+1].from - 1
				}
				for end > si.items[j].from && isBlankOrComment(si.lines[end]) {
					end--
				}
				si.items[j].to = end
				si.items[j].Text = dedentItem(si.lines[si.items[j].from : end+1])
			}
		}
		return si, nil
	}
	return si, nil
}

// dedentItem 去掉 "- " 与公共缩进，得到映射文本。
func dedentItem(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	first := lines[0]
	dash := strings.Index(first, "-")
	col := dash + 1
	for col < len(first) && first[col] == ' ' {
		col++
	}
	var b strings.Builder
	b.WriteString(first[col:])
	for _, l := range lines[1:] {
		if len(l) >= col && strings.TrimSpace(l[:col]) == "" {
			b.WriteString(l[col:])
		} else {
			b.WriteString(strings.TrimLeft(l, " \t"))
		}
	}
	out := b.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// indentItem 把映射文本变成序列项：第一行加 "- "，其余行对齐。
func indentItem(text string, indent int) string {
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	for i, l := range splitLines([]byte(strings.TrimRight(text, "\n") + "\n")) {
		switch {
		case i == 0:
			b.WriteString(pad + "- " + l)
		case strings.TrimSpace(l) == "":
			b.WriteString("\n")
		default:
			b.WriteString(pad + "  " + l)
		}
	}
	return b.String()
}

// ListItems 返回 key（pools / models）序列的各项。
func ListItems(data []byte, key string) ([]ListItem, error) {
	si, err := analyzeSeq(data, key)
	if err != nil {
		return nil, err
	}
	return si.items, nil
}

// EntryName 校验 text 是一个映射（一项），返回它的 name。
func EntryName(text string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("invalid YAML: %v", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("the entry must be a YAML mapping (name: ..., ...)")
	}
	_, nv := lookup(doc.Content[0], "name")
	if nv == nil || nv.Kind != yaml.ScalarNode || strings.TrimSpace(nv.Value) == "" {
		return "", errors.New("the entry needs a name")
	}
	return nv.Value, nil
}

// ReplaceListItem 用 text 替换序列中名为 name 的项（PE-04）。
func ReplaceListItem(data []byte, key, name, text string) ([]byte, error) {
	si, err := analyzeSeq(data, key)
	if err != nil {
		return nil, err
	}
	for _, it := range si.items {
		if it.Name == name {
			out := strings.Join(si.lines[:it.from], "") + indentItem(text, si.indent) + strings.Join(si.lines[it.to+1:], "")
			return []byte(out), nil
		}
	}
	return nil, fmt.Errorf("%s %q not found", strings.TrimSuffix(key, "s"), name)
}

// AppendListItem 把 text 追加到序列末尾；序列不存在时在文件末尾新建（PE-04）。
func AppendListItem(data []byte, key, text string) ([]byte, error) {
	si, err := analyzeSeq(data, key)
	if err != nil {
		return nil, err
	}
	lines := si.lines
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n"
	}
	item := indentItem(text, si.indent)
	switch {
	case si.keyLine < 0:
		return []byte(strings.Join(lines, "") + key + ":\n" + item), nil
	case si.empty:
		head := key + ":\n"
		if c := strings.Index(lines[si.keyLine], "#"); c >= 0 && !si.flow {
			head = strings.TrimRight(lines[si.keyLine], "\n") + "\n"
		}
		return []byte(strings.Join(lines[:si.keyLine], "") + head + item + strings.Join(lines[si.keyLine+1:], "")), nil
	}
	at := si.items[len(si.items)-1].to + 1
	return []byte(strings.Join(lines[:at], "") + item + strings.Join(lines[at:], "")), nil
}

// RemoveListItem 删除序列中名为 name 的项（PE-04）。
func RemoveListItem(data []byte, key, name string) ([]byte, error) {
	si, err := analyzeSeq(data, key)
	if err != nil {
		return nil, err
	}
	for _, it := range si.items {
		if it.Name == name {
			return []byte(strings.Join(si.lines[:it.from], "") + strings.Join(si.lines[it.to+1:], "")), nil
		}
	}
	return nil, fmt.Errorf("%s %q not found", strings.TrimSuffix(key, "s"), name)
}

// ---------------------------------------------------------------- 表单与 YAML 的同步（PE-02 页面）

// EntryFields 是表单中的常用字段。池：name、protocol、base_url、strategy、max_attempts；
// 服务：name、pool、upstream_model、price（单位 → 价格）、reserve（单位 → 数量）。
type EntryFields map[string]any

// ReadEntryFields 从一项的 YAML 读出表单字段。
func ReadEntryFields(kind, text string) (EntryFields, error) {
	f := EntryFields{}
	switch kind {
	case "pools":
		var p PoolConfig
		if err := yaml.Unmarshal([]byte(text), &p); err != nil {
			return nil, fmt.Errorf("invalid YAML: %v", err)
		}
		f["name"], f["protocol"], f["base_url"], f["strategy"], f["max_attempts"] = p.Name, p.Protocol, p.BaseURL, p.Strategy, p.MaxAttempts
	case "models":
		var m ModelConfig
		if err := yaml.Unmarshal([]byte(text), &m); err != nil {
			return nil, fmt.Errorf("invalid YAML: %v", err)
		}
		price := map[string]string{}
		for k, v := range m.Price {
			price[k] = v
		}
		reserve := map[string]int64{}
		for k, v := range m.Reserve {
			reserve[k] = v
		}
		f["name"], f["pool"], f["upstream_model"], f["price"], f["reserve"] = m.Name, m.Pool, m.UpstreamModel, price, reserve
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
	return f, nil
}

// ApplyEntryFields 把表单字段写进一项的 YAML：标量字段直接替换（空值删除该键），
// price / reserve 按单位整体替换；其他键与注释保留。text 为空时从空映射开始。
func ApplyEntryFields(kind, text string, f EntryFields) (string, error) {
	var doc yaml.Node
	if strings.TrimSpace(text) == "" {
		text = "{}"
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("invalid YAML: %v", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("the entry must be a YAML mapping")
	}
	m := doc.Content[0]
	m.Style = 0
	var scalars []string
	switch kind {
	case "pools":
		scalars = []string{"name", "protocol", "base_url", "strategy", "max_attempts"}
	case "models":
		scalars = []string{"name", "pool", "upstream_model"}
	default:
		return "", fmt.Errorf("unknown kind %q", kind)
	}
	for _, k := range scalars {
		v, ok := f[k]
		if !ok {
			continue
		}
		s := scalarString(v)
		if s == "" || s == "0" && k == "max_attempts" {
			removeKey(m, k)
			continue
		}
		setScalar(m, k, s, k == "max_attempts")
	}
	if kind == "models" {
		for _, k := range []string{"price", "reserve"} {
			v, ok := f[k]
			if !ok {
				continue
			}
			vals, ok := v.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%s must be an object", k)
			}
			setMap(m, k, vals, k == "reserve")
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func removeKey(m *yaml.Node, k string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func setScalar(m *yaml.Node, k, v string, number bool) {
	tag := "!!str"
	if number {
		tag = "!!int"
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			n := m.Content[i+1]
			if n.Kind == yaml.ScalarNode && n.Value == v {
				return
			}
			*n = yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v, LineComment: n.LineComment}
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v})
}

// setMap 把 m[k] 设为 vals（空值的单位去掉；全部为空时删除该键）；原来是流式写法时保持流式。
func setMap(m *yaml.Node, k string, vals map[string]any, number bool) {
	keys := make([]string, 0, len(vals))
	for u, v := range vals {
		if scalarString(v) != "" {
			keys = append(keys, u)
		}
	}
	sort.Strings(keys)
	style := yaml.FlowStyle
	_, old := lookup(m, k)
	if old != nil && old.Kind == yaml.MappingNode && old.Style&yaml.FlowStyle == 0 {
		style = 0
	}
	if len(keys) == 0 {
		removeKey(m, k)
		return
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: style}
	for _, u := range keys {
		tag, st := "!!str", yaml.DoubleQuotedStyle
		if number {
			tag, st = "!!int", 0
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: u},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: scalarString(vals[u]), Style: st})
	}
	if old != nil {
		n.LineComment = old.LineComment
		*old = *n
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, n)
}
