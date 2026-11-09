package translate

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

// respEmitter 生成 spec/10 §8.3 的 Responses 流式事件序列，与上游协议无关；
// T26（自 Anthropic）与 T27（自 Chat）的状态机把各自的上游事件翻译成对它的调用。非并发安全。
type respEmitter struct {
	model string
	now   time.Time
	newID idGen

	resp    respObject
	seq     int64
	queue   []sse.Event
	started bool
	done    bool

	text     *openText // 当前打开的 message 项
	tool     *openTool // 当前打开的 function_call 项
	finished []respOutputItem
}

type openText struct {
	id    string
	index int
	sb    strings.Builder
}

type openTool struct {
	id, callID, name string
	key              int // 上游的工具调用标识：Anthropic content block index / Chat tool_calls index
	index            int
	args             strings.Builder
}

func newRespEmitter(model string, now time.Time, newID idGen) respEmitter {
	return respEmitter{model: model, now: now, newID: newID}
}

// pop 取出下一个待输出事件。
func (s *respEmitter) pop() (sse.Event, bool) {
	if len(s.queue) == 0 {
		return sse.Event{}, false
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, true
}

// start 输出 response.created 与 response.in_progress（只输出一次）。
func (s *respEmitter) start() {
	if s.started {
		return
	}
	s.started = true
	s.resp = respObject{ID: s.newID("resp_"), Object: "response", CreatedAt: s.now.Unix(), Status: "in_progress",
		Model: s.model, Output: []respOutputItem{}}
	s.emit("response.created", map[string]any{"response": s.resp})
	s.emit("response.in_progress", map[string]any{"response": s.resp})
}

// nextIndex 是下一个输出项的 output_index：新项总是在关闭其他打开的项之后才打开，
// 因此等于已完成的项数（§8.3：按出现顺序从 0 编号）。
func (s *respEmitter) nextIndex() int { return len(s.finished) }

func (s *respEmitter) textDelta(delta string) {
	if delta == "" {
		return
	}
	if s.text == nil {
		s.closeTool()
		s.text = &openText{id: s.newID("msg_"), index: s.nextIndex()}
		s.emit("response.output_item.added", map[string]any{"output_index": s.text.index, "item": respOutputItem{
			Type: "message", ID: s.text.id, Status: "in_progress", Role: "assistant", Content: []respContentPart{},
		}})
		s.emit("response.content_part.added", map[string]any{"item_id": s.text.id, "output_index": s.text.index,
			"content_index": 0, "part": respContentPart{Type: "output_text", Annotations: []any{}}})
	}
	s.text.sb.WriteString(delta)
	s.emit("response.output_text.delta", map[string]any{"item_id": s.text.id, "output_index": s.text.index,
		"content_index": 0, "delta": delta})
}

func (s *respEmitter) closeText() {
	if s.text == nil {
		return
	}
	t := s.text
	s.text = nil
	part := respContentPart{Type: "output_text", Text: t.sb.String(), Annotations: []any{}}
	item := respOutputItem{Type: "message", ID: t.id, Status: "completed", Role: "assistant", Content: []respContentPart{part}}
	s.emit("response.output_text.done", map[string]any{"item_id": t.id, "output_index": t.index, "content_index": 0, "text": part.Text})
	s.emit("response.content_part.done", map[string]any{"item_id": t.id, "output_index": t.index, "content_index": 0, "part": part})
	s.emit("response.output_item.done", map[string]any{"output_index": t.index, "item": item})
	s.finished = append(s.finished, item)
}

// openTool 关闭打开的项后开始一个 function_call 项。
func (s *respEmitter) openTool(key int, callID, name string) {
	s.closeText()
	s.closeTool()
	s.tool = &openTool{id: s.newID("fc_"), callID: callID, name: name, key: key, index: s.nextIndex()}
	empty := ""
	s.emit("response.output_item.added", map[string]any{"output_index": s.tool.index, "item": respOutputItem{
		Type: "function_call", ID: s.tool.id, Status: "in_progress", CallID: callID, Name: name, Arguments: &empty,
	}})
}

// toolArgs 追加打开的工具调用的参数增量；key 不是当前打开的工具调用时返回 false。
func (s *respEmitter) toolArgs(key int, partial string) bool {
	if s.tool == nil || s.tool.key != key {
		return false
	}
	if partial == "" {
		return true
	}
	s.tool.args.WriteString(partial)
	s.emit("response.function_call_arguments.delta", map[string]any{
		"item_id": s.tool.id, "output_index": s.tool.index, "delta": partial,
	})
	return true
}

func (s *respEmitter) closeTool() {
	if s.tool == nil {
		return
	}
	t := s.tool
	s.tool = nil
	args := t.args.String()
	if args == "" {
		args = "{}" // 无参数工具：上游可能不发送参数增量
	}
	item := respOutputItem{Type: "function_call", ID: t.id, Status: "completed", CallID: t.callID, Name: t.name, Arguments: &args}
	s.emit("response.function_call_arguments.done", map[string]any{"item_id": t.id, "output_index": t.index, "arguments": args})
	s.emit("response.output_item.done", map[string]any{"output_index": t.index, "item": item})
	s.finished = append(s.finished, item)
}

// finish 关闭打开的项，输出 response.completed（或 response.incomplete），之后流结束。
func (s *respEmitter) finish(status string, incomplete *respIncomplete, usage *respUsage) {
	s.start()
	s.closeText()
	s.closeTool()
	final := s.resp
	final.Status, final.IncompleteDetails = status, incomplete
	final.Output = s.finished
	if final.Output == nil {
		final.Output = []respOutputItem{}
	}
	final.Usage = usage
	name := "response.completed"
	if status == "incomplete" {
		name = "response.incomplete"
	}
	s.emit(name, map[string]any{"response": final})
	s.done = true
}

// emit 追加一个事件：event 名与 data.type 相同，sequence_number 从 0 递增（§8.3）。
func (s *respEmitter) emit(typ string, fields map[string]any) {
	fields["type"] = typ
	fields["sequence_number"] = s.seq
	s.seq++
	b, err := json.Marshal(fields)
	if err != nil { // 字段全部是本包定义的可序列化类型，不会失败
		b = []byte(`{"type":"` + typ + `"}`)
	}
	s.queue = append(s.queue, sse.Event{Name: typ, Data: b})
}
