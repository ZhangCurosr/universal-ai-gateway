// Package sse 实现 Server-Sent Events 的读取与写入（spec/08 §6）。
//
// 读取规则遵循 WHATWG 规范的子集：
//   - 以空行结束一个事件；
//   - "event:" 设置事件名，"data:" 追加一行数据（多行用 \n 连接），"id:" 设置 ID；
//   - 冒号后若紧跟一个空格则去掉该空格；
//   - 以 ":" 开头的行是注释，忽略；
//   - 支持 \n 与 \r\n 换行。
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
)

// MaxLineBytes 是单行最大长度。超过时 Next 返回 ErrLineTooLong。
const MaxLineBytes = 4 << 20

// ErrLineTooLong 表示上游发送了超长的行。
var ErrLineTooLong = errors.New("sse: line too long")

// Event 是一个 SSE 事件。Data 不含末尾换行。
type Event struct {
	Name string // "event:" 字段；缺省为空
	ID   string
	Data []byte
}

// Reader 从流中逐个读取事件。非并发安全。
type Reader struct {
	sc *bufio.Scanner
}

// NewReader 创建读取器。
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	sc.Split(scanLines)
	return &Reader{sc: sc}
}

// Next 返回下一个事件。流正常结束返回 io.EOF；
// 流在事件中途结束时，若已累积数据则先返回该事件，下一次调用返回 io.EOF。
func (r *Reader) Next() (Event, error) {
	var ev Event
	var data bytes.Buffer
	hasField := false
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			if hasField {
				ev.Data = append([]byte(nil), data.Bytes()...)
				return ev, nil
			}
			continue // 连续空行
		}
		if line[0] == ':' {
			continue // 注释
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "event":
			ev.Name = string(value)
			hasField = true
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasField = true
		case "id":
			ev.ID = string(value)
			hasField = true
		default:
			// 未知字段（如 retry）忽略
		}
	}
	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, ErrLineTooLong
		}
		return Event{}, err
	}
	if hasField {
		ev.Data = append([]byte(nil), data.Bytes()...)
		return ev, nil
	}
	return Event{}, io.EOF
}

// scanLines 与 bufio.ScanLines 相同，但同时去掉 \r\n 中的 \r。
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	return bufio.ScanLines(data, atEOF)
}

// Writer 向客户端写事件，每个事件后 Flush。非并发安全。
type Writer struct {
	w io.Writer
	f http.Flusher
}

// NewWriter 创建写入器。w 实现 http.Flusher 时每个事件后自动 Flush。
func NewWriter(w io.Writer) *Writer {
	f, _ := w.(http.Flusher)
	return &Writer{w: w, f: f}
}

// WriteEvent 写一个事件并 Flush。Data 中的每一行各写成一个 "data:" 行。
func (w *Writer) WriteEvent(ev Event) error {
	var buf bytes.Buffer
	if ev.Name != "" {
		buf.WriteString("event: ")
		buf.WriteString(ev.Name)
		buf.WriteByte('\n')
	}
	if ev.ID != "" {
		buf.WriteString("id: ")
		buf.WriteString(ev.ID)
		buf.WriteByte('\n')
	}
	for _, line := range bytes.Split(ev.Data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := w.w.Write(buf.Bytes()); err != nil {
		return err
	}
	if w.f != nil {
		w.f.Flush()
	}
	return nil
}
