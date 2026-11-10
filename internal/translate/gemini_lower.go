package translate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/ir"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// geminiTopLevel 是 Gemini 请求体中本转换器认识的顶层字段（spec/10 §10.3）。
// safetySettings 只对 Gemini 有意义，按规格丢弃；cachedContent 始终 400。
var geminiTopLevel = map[string]bool{
	"contents": true, "systemInstruction": true, "tools": true, "toolConfig": true,
	"generationConfig": true, "safetySettings": true, "cachedContent": true,
}

// geminiGenConfig 是 generationConfig 下认识的字段；按顶层字段对待（§10.3 最后一行）。
var geminiGenConfig = map[string]bool{
	"maxOutputTokens": true, "temperature": true, "topP": true, "stopSequences": true, "candidateCount": true,
	"topK": true, "thinkingConfig": true, "mediaResolution": true, "responseMimeType": true, "responseModalities": true,
}

// geminiGenRejected 是 generationConfig 下始终 400 的字段（改变采样语义、输出形状或要求结构化 / 多模态输出）。
var geminiGenRejected = []string{
	"seed", "presencePenalty", "frequencyPenalty", "responseLogprobs", "logprobs",
	"responseSchema", "responseJsonSchema", "responseFormat", "speechConfig", "imageConfig",
}

// geminiRejectedParts 是明确不支持的 part 类型（§10.3）。
var geminiRejectedParts = []string{"executableCode", "codeExecutionResult", "videoMetadata", "fileData"}

// geminiContentKeys 是 contents[] 元素中认识的字段。
var geminiContentKeys = map[string]bool{"role": true, "parts": true}

type geminiInPart struct {
	Text             *string         `json:"text"`
	Thought          bool            `json:"thought"`
	ThoughtSignature string          `json:"thoughtSignature"`
	InlineData       json.RawMessage `json:"inlineData"`
	FunctionCall     json.RawMessage `json:"functionCall"`
	FunctionResponse json.RawMessage `json:"functionResponse"`
}

// geminiCallRef 记录一个 functionCall 的 ID 与是否已被 functionResponse 对应（GID-04）。
type geminiCallRef struct {
	id, name string
	matched  bool
}

// lowerGemini 把 Gemini generateContent / streamGenerateContent 请求解析为 IR（spec/10 §10.3）。
func lowerGemini(env *protocol.Envelope, opts Options, from, to string) (*ir.Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(env.Body, &top); err != nil {
		return nil, invalid("request body must be a JSON object")
	}
	if present(top["cachedContent"]) {
		return nil, invalid(fmt.Sprintf("cachedContent is not supported for model %q", env.Model))
	}
	if err := checkUnknown(top, geminiTopLevel, "", opts, from, to); err != nil {
		return nil, err
	}

	req := &ir.Request{Model: env.Model, Stream: env.Stream}
	if err := lowerGeminiSystem(top["systemInstruction"], req); err != nil {
		return nil, err
	}
	if err := lowerGeminiContents(top["contents"], req, env.Model, opts, from, to); err != nil {
		return nil, err
	}
	if err := lowerGeminiTools(top["tools"], req, env.Model); err != nil {
		return nil, err
	}
	if err := lowerGeminiToolConfig(top["toolConfig"], req); err != nil {
		return nil, err
	}
	if err := lowerGeminiGenConfig(top["generationConfig"], req, opts, from, to); err != nil {
		return nil, err
	}
	return req, nil
}

// acceptGemini 实现 GL-01：转译路径只支持 generateContent / streamGenerateContent。
func acceptGemini(env *protocol.Envelope) error {
	switch env.Params["method"] {
	case "generateContent", "streamGenerateContent":
		return nil
	default:
		return invalid(fmt.Sprintf("%s is not supported for model %q", env.Params["method"], env.Model))
	}
}

// checkUnknown 按 spec/10 §1 第 5、6 条处理 obj 中不在 known 里的字段；prefix 是字段路径前缀（如 "generationConfig."）。
func checkUnknown(obj map[string]json.RawMessage, known map[string]bool, prefix string, opts Options, from, to string) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 多个未知字段时报告稳定
	for _, k := range keys {
		if known[k] || !present(obj[k]) {
			continue
		}
		if !opts.DropUnknownFields {
			return invalid(prefix + k + " is not supported when translating to another protocol")
		}
		logDroppedField(opts, from, to, prefix+k)
	}
	return nil
}

func lowerGeminiSystem(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var sys struct {
		Parts []map[string]json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &sys); err != nil {
		return invalid("systemInstruction must be a Content object")
	}
	for i, p := range sys.Parts {
		path := fmt.Sprintf("systemInstruction.parts[%d]", i)
		for k := range p {
			if k != "text" && present(p[k]) {
				return invalid(path + ": only text parts are supported in systemInstruction")
			}
		}
		var text string
		if err := decodeString(p["text"], path+".text", &text); err != nil {
			return err
		}
		if text != "" {
			req.System = append(req.System, ir.TextPart{Text: text})
		}
	}
	return nil
}

func lowerGeminiContents(raw json.RawMessage, req *ir.Request, model string, opts Options, from, to string) error {
	var contents []map[string]json.RawMessage
	if !present(raw) {
		return invalid("contents: required")
	}
	if err := json.Unmarshal(raw, &contents); err != nil {
		return invalid("contents must be an array")
	}
	if len(contents) == 0 {
		return invalid("contents must not be empty")
	}
	callSeq := 0                  // GID-03：整个 contents 中 functionCall 的序号
	var lastModel []geminiCallRef // 紧邻的上一条 model 消息中的调用（GID-04）
	allIDs := map[string]bool{}   // 历史中出现过的调用 ID
	for i, c := range contents {
		path := fmt.Sprintf("contents[%d]", i)
		if err := checkUnknown(c, geminiContentKeys, "contents[].", opts, from, to); err != nil {
			return err
		}
		var role string
		if err := decodeString(c["role"], path+".role", &role); err != nil {
			return err
		}
		var msg ir.Message
		switch role {
		case "", "user", "function":
			msg.Role = ir.RoleUser
		case "model":
			msg.Role = ir.RoleAssistant
		default:
			return invalid(fmt.Sprintf("%s.role %q is not supported", path, role))
		}
		var parts []json.RawMessage
		if present(c["parts"]) {
			if err := json.Unmarshal(c["parts"], &parts); err != nil {
				return invalid(path + ".parts must be an array")
			}
		}
		var calls []geminiCallRef
		var results []ir.Part
		for j, rawPart := range parts {
			pp := fmt.Sprintf("%s.parts[%d]", path, j)
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(rawPart, &keys); err != nil {
				return invalid(pp + " must be an object")
			}
			for _, k := range geminiRejectedParts {
				if present(keys[k]) {
					return invalid(fmt.Sprintf("%s: part type %s is not supported for model %q", pp, k, model))
				}
			}
			var p geminiInPart
			if err := json.Unmarshal(rawPart, &p); err != nil {
				return invalid(pp + " is malformed")
			}
			switch {
			case p.Text != nil:
				if p.Thought {
					if msg.Role != ir.RoleAssistant {
						return invalid(pp + ": thought parts are only allowed in model messages")
					}
					msg.Parts = append(msg.Parts, ir.ThinkingPart{Vendor: ir.VendorGemini, Text: *p.Text, Signature: p.ThoughtSignature})
				} else if *p.Text != "" {
					msg.Parts = append(msg.Parts, ir.TextPart{Text: *p.Text}) // 非思考 part 上的 thoughtSignature 丢弃
				}
			case present(p.InlineData):
				img, err := lowerGeminiInline(p.InlineData, pp, model)
				if err != nil {
					return err
				}
				msg.Parts = append(msg.Parts, img)
			case present(p.FunctionCall):
				if msg.Role != ir.RoleAssistant {
					return invalid(pp + ": functionCall is only allowed in model messages")
				}
				tc, err := lowerGeminiCall(p.FunctionCall, pp, callSeq)
				if err != nil {
					return err
				}
				callSeq++
				allIDs[tc.ID] = true
				calls = append(calls, geminiCallRef{id: tc.ID, name: tc.Name})
				msg.Parts = append(msg.Parts, tc)
			case present(p.FunctionResponse):
				if msg.Role != ir.RoleUser {
					return invalid(pp + ": functionResponse is only allowed in user messages")
				}
				tr, err := lowerGeminiResponse(p.FunctionResponse, pp, lastModel, allIDs)
				if err != nil {
					return err
				}
				results = append(results, tr)
			case p.ThoughtSignature != "" && onlyKeys(keys, "thoughtSignature", "thought"):
				// 只携带签名的空 part：签名只能回传给 Gemini（§1 第 3 条），跨厂商丢弃
			default:
				return invalid(pp + ": part has no supported content (text, inlineData, functionCall, functionResponse)")
			}
		}
		if len(results) > 0 {
			msg.Parts = append(results, msg.Parts...) // 工具结果放在该消息其余内容之前
		}
		if msg.Role == ir.RoleAssistant {
			lastModel = calls
		}
		if len(msg.Parts) > 0 {
			req.Messages = append(req.Messages, msg)
		}
	}
	if len(req.Messages) == 0 {
		return invalid("contents must contain at least one non-empty message")
	}
	return nil
}

// onlyKeys 报告 obj 中值不为 null 的键都在 allowed 里。
func onlyKeys(obj map[string]json.RawMessage, allowed ...string) bool {
	for k, v := range obj {
		if present(v) && !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

func lowerGeminiInline(raw json.RawMessage, path, model string) (ir.ImagePart, error) {
	var in struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ir.ImagePart{}, invalid(path + ".inlineData is malformed")
	}
	if !strings.HasPrefix(in.MimeType, "image/") {
		return ir.ImagePart{}, invalid(fmt.Sprintf("inlineData mimeType %q is not supported for model %q", in.MimeType, model))
	}
	data, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil {
		return ir.ImagePart{}, invalid(path + ".inlineData.data is not valid base64")
	}
	return ir.ImagePart{MediaType: in.MimeType, Data: data}, nil
}

// lowerGeminiCall 实现 functionCall → ToolCallPart（GID-03）。
func lowerGeminiCall(raw json.RawMessage, path string, seq int) (ir.ToolCallPart, error) {
	var fc struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &fc); err != nil {
		return ir.ToolCallPart{}, invalid(path + ".functionCall is malformed")
	}
	if fc.Name == "" {
		return ir.ToolCallPart{}, invalid(path + ".functionCall.name: required")
	}
	args := json.RawMessage(`{}`)
	if present(fc.Args) {
		t := bytes.TrimSpace(fc.Args)
		if t[0] != '{' {
			return ir.ToolCallPart{}, invalid(path + ".functionCall.args must be an object")
		}
		args = json.RawMessage(t)
	}
	id := fc.ID
	if id == "" {
		id = fmt.Sprintf("gemini_call_%d", seq)
	}
	return ir.ToolCallPart{ID: id, Name: fc.Name, Arguments: args}, nil
}

// lowerGeminiResponse 实现 functionResponse → ToolResultPart（GID-04）。
func lowerGeminiResponse(raw json.RawMessage, path string, lastModel []geminiCallRef, allIDs map[string]bool) (ir.ToolResultPart, error) {
	var fr struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
		Parts    json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &fr); err != nil {
		return ir.ToolResultPart{}, invalid(path + ".functionResponse is malformed")
	}
	if present(fr.Parts) && !bytes.Equal(bytes.TrimSpace(fr.Parts), []byte("[]")) {
		return ir.ToolResultPart{}, invalid(path + ".functionResponse.parts is not supported when translating to another protocol")
	}
	id := fr.ID
	if id != "" {
		if !allIDs[id] {
			return ir.ToolResultPart{}, invalid(fmt.Sprintf("functionResponse %q has no matching functionCall", fr.Name))
		}
		for i := range lastModel {
			if lastModel[i].id == id {
				lastModel[i].matched = true
			}
		}
	} else {
		i := slices.IndexFunc(lastModel, func(c geminiCallRef) bool { return !c.matched && c.name == fr.Name })
		if i < 0 {
			return ir.ToolResultPart{}, invalid(fmt.Sprintf("functionResponse %q has no matching functionCall", fr.Name))
		}
		lastModel[i].matched = true
		id = lastModel[i].id
	}
	content := []byte(`{}`)
	if present(fr.Response) {
		var buf bytes.Buffer
		if err := json.Compact(&buf, fr.Response); err != nil {
			return ir.ToolResultPart{}, invalid(path + ".functionResponse.response must be valid JSON")
		}
		content = buf.Bytes()
	}
	return ir.ToolResultPart{ToolCallID: id, Content: []ir.Part{ir.TextPart{Text: string(content)}}}, nil
}

func lowerGeminiTools(raw json.RawMessage, req *ir.Request, model string) error {
	if !present(raw) {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return invalid("tools must be an array")
	}
	for i, t := range tools {
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k != "functionDeclarations" && present(t[k]) {
				return invalid(fmt.Sprintf("built-in tool %q is not supported for model %q", k, model))
			}
		}
		if !present(t["functionDeclarations"]) {
			continue
		}
		var decls []struct {
			Name                 string          `json:"name"`
			Description          string          `json:"description"`
			Parameters           json.RawMessage `json:"parameters"`
			ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
		}
		if err := json.Unmarshal(t["functionDeclarations"], &decls); err != nil {
			return invalid(fmt.Sprintf("tools[%d].functionDeclarations must be an array of objects", i))
		}
		for j, d := range decls {
			if d.Name == "" {
				return invalid(fmt.Sprintf("tools[%d].functionDeclarations[%d].name: required", i, j))
			}
			tool := ir.Tool{Name: d.Name, Description: d.Description}
			switch {
			case present(d.ParametersJSONSchema):
				tool.InputSchema = d.ParametersJSONSchema
			case present(d.Parameters):
				s, err := openAPISchemaToJSON(d.Parameters)
				if err != nil {
					return invalid(fmt.Sprintf("tools[%d].functionDeclarations[%d].parameters: %v", i, j, err))
				}
				tool.InputSchema = s
			default:
				tool.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			req.Tools = append(req.Tools, tool)
		}
	}
	return nil
}

// openAPISchemaToJSON 实现 GL-03：type 小写；nullable → type 数组；删除 propertyOrdering；其余键原样保留。
func openAPISchemaToJSON(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留数字原文（如 minimum、maxLength）
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not valid JSON")
	}
	out, err := json.Marshal(convertOpenAPISchema(v))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func convertOpenAPISchema(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "propertyOrdering" || k == "nullable" {
				continue
			}
			out[k] = convertOpenAPISchema(val)
		}
		if t, ok := x["type"].(string); ok {
			out["type"] = strings.ToLower(t)
		}
		if n, ok := x["nullable"].(bool); ok && n {
			if t, ok := out["type"].(string); ok {
				out["type"] = []any{t, "null"}
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = convertOpenAPISchema(e)
		}
		return out
	default:
		return v
	}
}

func lowerGeminiToolConfig(raw json.RawMessage, req *ir.Request) error {
	if !present(raw) {
		return nil
	}
	var tc struct {
		FunctionCallingConfig *struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return invalid("toolConfig must be an object")
	}
	if tc.FunctionCallingConfig == nil {
		return nil
	}
	fcc := tc.FunctionCallingConfig
	if len(fcc.AllowedFunctionNames) > 0 && fcc.Mode != "ANY" {
		return invalid("toolConfig.functionCallingConfig.allowedFunctionNames is only supported with mode ANY")
	}
	switch fcc.Mode {
	case "AUTO", "MODE_UNSPECIFIED", "":
		// SPEC-GAP(T48): MODE_UNSPECIFIED 与缺省按 Gemini 文档的默认值 AUTO 处理（§10.3 只列了 AUTO / ANY / NONE）
		req.ToolChoice = &ir.ToolChoice{Mode: "auto"}
	case "NONE":
		req.ToolChoice = &ir.ToolChoice{Mode: "none"}
	case "ANY":
		switch len(fcc.AllowedFunctionNames) {
		case 0:
			req.ToolChoice = &ir.ToolChoice{Mode: "any"}
		case 1:
			req.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: fcc.AllowedFunctionNames[0]}
		default:
			return invalid("toolConfig.functionCallingConfig.allowedFunctionNames with more than one name is not supported when translating to another protocol")
		}
	default:
		return invalid(fmt.Sprintf("toolConfig.functionCallingConfig.mode %q is not supported when translating to another protocol", fcc.Mode))
	}
	return nil
}

func lowerGeminiGenConfig(raw json.RawMessage, req *ir.Request, opts Options, from, to string) error {
	if !present(raw) {
		return nil
	}
	var gc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gc); err != nil {
		return invalid("generationConfig must be an object")
	}
	for _, k := range geminiGenRejected {
		if present(gc[k]) {
			return invalid("generationConfig." + k + " is not supported when translating to another protocol")
		}
	}
	if err := checkUnknown(gc, geminiGenConfig, "generationConfig.", opts, from, to); err != nil {
		return err
	}
	if present(gc["candidateCount"]) {
		var n int64
		if err := decodeInt(gc["candidateCount"], "generationConfig.candidateCount", &n); err != nil {
			return err
		}
		if n > 1 {
			return invalid("generationConfig.candidateCount > 1 is not supported")
		}
	}
	if present(gc["responseMimeType"]) {
		var mt string
		if err := decodeString(gc["responseMimeType"], "generationConfig.responseMimeType", &mt); err != nil {
			return err
		}
		if mt != "" && mt != "text/plain" {
			return invalid(fmt.Sprintf("generationConfig.responseMimeType %q is not supported when translating to another protocol", mt))
		}
	}
	if present(gc["responseModalities"]) {
		var mods []string
		if err := json.Unmarshal(gc["responseModalities"], &mods); err != nil {
			return invalid("generationConfig.responseModalities must be an array of strings")
		}
		for _, m := range mods {
			if m != "TEXT" {
				return invalid(fmt.Sprintf("generationConfig.responseModalities %q is not supported when translating to another protocol", m))
			}
		}
	}
	if present(gc["maxOutputTokens"]) {
		if err := decodeInt(gc["maxOutputTokens"], "generationConfig.maxOutputTokens", &req.MaxTokens); err != nil {
			return err
		}
		if req.MaxTokens <= 0 {
			return invalid("generationConfig.maxOutputTokens must be a positive integer")
		}
	}
	for _, f := range []struct {
		key string
		dst **float64
	}{{"temperature", &req.Temperature}, {"topP", &req.TopP}} {
		if present(gc[f.key]) {
			var v float64
			if err := json.Unmarshal(gc[f.key], &v); err != nil {
				return invalid("generationConfig." + f.key + " must be a number")
			}
			*f.dst = &v
		}
	}
	if present(gc["stopSequences"]) {
		if err := json.Unmarshal(gc["stopSequences"], &req.StopSequences); err != nil {
			return invalid("generationConfig.stopSequences must be an array of strings")
		}
	}
	// topK、thinkingConfig、mediaResolution：按 §10.3 丢弃
	return nil
}
