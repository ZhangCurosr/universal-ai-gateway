package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxConfigFileBytes 是网页修改配置文件时读取的上限（LAN-03）。
const MaxConfigFileBytes = 4 << 20

// ErrUnchanged 表示要写入的值与文件中现有的值相同（LAN-02：不写文件）。
var ErrUnchanged = errors.New("config: value unchanged")

// SetScalar 把 YAML 文本 data 中 keys 路径（如 server、data_listen）处的值改为字符串 value（LAN-03）。
// 只改动这一处文本：注释、空行、缩进与其他字段保持原样。路径上的映射必须是块样式；
// 字段不存在时插到父映射的第一行之前（父映射不存在时追加到文件末尾）。返回新文本与原来的值（不存在时为空）。
func SetScalar(data []byte, keys []string, value string) ([]byte, string, error) {
	if len(keys) == 0 {
		return nil, "", errors.New("config: empty key path")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, "", fmt.Errorf("config: parse yaml: %w", err)
	}
	quoted := strconv.Quote(value)
	lines := strings.SplitAfter(string(data), "\n")
	if len(doc.Content) == 0 { // 空文件
		return []byte(appendLines(string(data), keys, quoted, 0)), "", nil
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, "", errors.New("config: the top level of the file is not a mapping")
	}
	for i, k := range keys {
		if m.Style&yaml.FlowStyle != 0 {
			return nil, "", fmt.Errorf("config: %s is written in flow style ({...}); edit it by hand", strings.Join(keys[:i], "."))
		}
		keyNode, valNode := lookup(m, k)
		last := i == len(keys)-1
		if keyNode == nil { // 字段不存在：插入剩余路径
			if m == doc.Content[0] {
				return []byte(appendLines(string(data), keys[i:], quoted, 0)), "", nil
			}
			indent := childIndent(m)
			return []byte(insertAt(lines, m.Content[0].Line-1, keys[i:], quoted, indent)), "", nil
		}
		if last {
			if valNode.Kind != yaml.ScalarNode || valNode.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || valNode.Line != keyNode.Line {
				return nil, "", fmt.Errorf("config: %s is not a single-line value; edit it by hand", strings.Join(keys, "."))
			}
			out, err := replaceValue(lines, valNode, quoted)
			if err != nil {
				return nil, "", err
			}
			return []byte(out), valNode.Value, nil
		}
		switch {
		case valNode.Kind == yaml.MappingNode && len(valNode.Content) > 0:
			m = valNode
		case valNode.Kind == yaml.ScalarNode && valNode.Tag == "!!null" && valNode.Value == "":
			// 「server:」后面什么都没有：插在它的下一行
			indent := keyNode.Column - 1 + 2
			return []byte(insertAt(lines, keyNode.Line, keys[i+1:], quoted, indent)), "", nil
		default:
			return nil, "", fmt.Errorf("config: %s is not a mapping; edit it by hand", strings.Join(keys[:i+1], "."))
		}
	}
	return nil, "", errors.New("config: unreachable")
}

func lookup(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// childIndent 是块映射中键的缩进（列号从 1 开始）。
func childIndent(m *yaml.Node) int { return m.Content[0].Column - 1 }

// nested 生成 keys 路径的块样式文本，第一层缩进 indent 个空格。
func nested(keys []string, quoted string, indent int) string {
	var b strings.Builder
	for i, k := range keys {
		b.WriteString(strings.Repeat(" ", indent+2*i))
		b.WriteString(k)
		b.WriteString(":")
		if i == len(keys)-1 {
			b.WriteString(" " + quoted)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func appendLines(s string, keys []string, quoted string, indent int) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s + nested(keys, quoted, indent)
}

// insertAt 在第 at 行（从 0 开始）之前插入 keys 路径的文本。
func insertAt(lines []string, at int, keys []string, quoted string, indent int) string {
	if at > len(lines) {
		at = len(lines)
	}
	if at > 0 && !strings.HasSuffix(lines[at-1], "\n") {
		lines[at-1] += "\n"
	}
	return strings.Join(lines[:at], "") + nested(keys, quoted, indent) + strings.Join(lines[at:], "")
}

// replaceValue 把 v 所在行中值的原文换成 quoted，保留行内注释。
func replaceValue(lines []string, v *yaml.Node, quoted string) (string, error) {
	li := v.Line - 1
	if li < 0 || li >= len(lines) {
		return "", errors.New("config: value position out of range")
	}
	line := []rune(lines[li])
	start := v.Column - 1
	if start < 0 || start > len(line) {
		return "", errors.New("config: value position out of range")
	}
	end := start
	switch {
	case v.Style&yaml.DoubleQuotedStyle != 0:
		end = start + 1
		for end < len(line) && line[end] != '"' {
			if line[end] == '\\' {
				end++
			}
			end++
		}
		end++
	case v.Style&yaml.SingleQuotedStyle != 0:
		end = start + 1
		for end < len(line) {
			if line[end] == '\'' {
				if end+1 < len(line) && line[end+1] == '\'' {
					end += 2
					continue
				}
				break
			}
			end++
		}
		end++
	default: // 普通标量：到行内注释（空白 + #）或行尾
		end = len(line)
		for i := start; i < len(line); i++ {
			if line[i] == '#' && i > start && (line[i-1] == ' ' || line[i-1] == '\t') {
				end = i
				break
			}
		}
		for end > start && strings.ContainsRune(" \t\r\n", line[end-1]) {
			end--
		}
	}
	if end > len(line) || end <= start {
		return "", errors.New("config: could not locate the value text")
	}
	lines[li] = string(line[:start]) + quoted + string(line[end:])
	return strings.Join(lines, ""), nil
}

// EditFile 实现 LAN-03：读取 path（≤ MaxConfigFileBytes），把 keys 处改为 value，用 Parse 校验新内容，
// 把原内容备份到 path + ".bak"，再以临时文件 + rename 写入。返回原内容（供调用者回滚）与原来的值。
// 值没有变化时返回 ErrUnchanged，不写任何文件。
func EditFile(path string, keys []string, value string) (orig []byte, old string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	orig, err = io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	_ = f.Close() // 只读文件
	if err != nil {
		return nil, "", err
	}
	if len(orig) > MaxConfigFileBytes {
		return nil, "", fmt.Errorf("config: %s is larger than %d bytes", filepath.Base(path), MaxConfigFileBytes)
	}
	out, old, err := SetScalar(orig, keys, value)
	if err != nil {
		return nil, "", err
	}
	if bytes.Equal(out, orig) || old == value {
		return orig, old, ErrUnchanged
	}
	if _, err := Parse(out); err != nil {
		return nil, "", fmt.Errorf("config: the edited file would be invalid: %w", err)
	}
	if err := WriteFileAtomic(path+".bak", orig); err != nil {
		return nil, "", fmt.Errorf("config: write backup: %w", err)
	}
	if err := WriteFileAtomic(path, out); err != nil {
		return nil, "", err
	}
	return orig, old, nil
}

// WriteFileAtomic 以临时文件 + rename 写入 path，保留原文件的权限（不存在时 0600）。
func WriteFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name) // 清理临时文件；失败无影响
		return werr
	}
	return nil
}
