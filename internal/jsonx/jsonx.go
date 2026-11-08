// Package jsonx 提供 JSON 的顶层字段局部读写（spec/08 §3）。
//
// 透传路径禁止把请求体整体反序列化为结构体再序列化；只能用本包改写顶层字段，
// 这样每个值内部的原始字节（cache_control、未知字段、数字精度）保持不变。
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

// ErrNotObject 表示输入不是 JSON 对象。
var ErrNotObject = errors.New("jsonx: body is not a JSON object")

// decodeObject 把顶层对象解为 键 → 原始值。
func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ErrNotObject
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, ErrNotObject
	}
	return m, nil
}

// SetTopLevel 返回把顶层键 key 设为 value（必须是合法 JSON）后的新文档。
// 顶层键会按字母序重排，每个值内部字节保持不变。
func SetTopLevel(body []byte, key string, value json.RawMessage) ([]byte, error) {
	m, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	if !json.Valid(value) {
		return nil, errors.New("jsonx: value is not valid JSON")
	}
	m[key] = value
	return encodeObject(m)
}

// encodeObject 按键的字母序输出对象，值的原始字节原样写入。
// 不用 json.Marshal：它会压缩 RawMessage 中的空白并转义 <、>、&，改变值的字节。
func encodeObject(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := enc.Encode(k); err != nil { // Encode 会追加换行，下面去掉
			return nil, err
		}
		buf.Truncate(buf.Len() - 1)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(m[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// SetTopLevelString 是 SetTopLevel 的字符串便捷版本。
func SetTopLevelString(body []byte, key, value string) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return SetTopLevel(body, key, raw)
}

// Get 沿着对象键路径取原始值；任一层不是对象或键不存在返回 ok=false。
// 值为 JSON null 时也返回 ok=false。
func Get(body []byte, path ...string) (json.RawMessage, bool) {
	cur := json.RawMessage(body)
	for _, k := range path {
		m, err := decodeObject(cur)
		if err != nil {
			return nil, false
		}
		v, ok := m[k]
		if !ok {
			return nil, false
		}
		cur = v
	}
	if bytes.Equal(bytes.TrimSpace(cur), []byte("null")) {
		return nil, false
	}
	return cur, true
}

// GetString 取字符串值；不存在或类型不符返回 ("", false)。
func GetString(body []byte, path ...string) (string, bool) {
	raw, ok := Get(body, path...)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// GetBool 取布尔值；不存在或类型不符返回 (false, false)。
func GetBool(body []byte, path ...string) (bool, bool) {
	raw, ok := Get(body, path...)
	if !ok {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

// GetInt64 取整数值；不存在、不是整数或越界返回 (0, false)。
func GetInt64(body []byte, path ...string) (int64, bool) {
	raw, ok := Get(body, path...)
	if !ok {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// ArrayElem 取数组 arr 的第 i 个元素原始值；不是数组或越界返回 ok=false。
func ArrayElem(arr json.RawMessage, i int) (json.RawMessage, bool) {
	var elems []json.RawMessage
	if err := json.Unmarshal(arr, &elems); err != nil || i < 0 || i >= len(elems) {
		return nil, false
	}
	return elems[i], true
}
