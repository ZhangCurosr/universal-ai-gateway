package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// 工具列表模式（spec/14 CAT-02，取值与 config mcp.tool_mode 相同）。空串等价于 ToolModeDirect。
const (
	ToolModeAuto    = "auto"
	ToolModeDirect  = "direct"
	ToolModeCatalog = "catalog"
)

// 目录工具名（CAT-01）。它们由本包提供，不进入注册表。
const (
	CatalogSearchTool   = "gateway_search_tools"
	CatalogDescribeTool = "gateway_describe_tool"
	CatalogCallTool     = "gateway_call_tool"
)

// defaultAutoThreshold 是 Deps.AutoThreshold 未设置（<= 0）时使用的阈值。
const defaultAutoThreshold = 8

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 100
	summaryMaxRunes    = 160
)

// IsCatalogTool 报告 name 是否是目录工具名（CAT-01）。
func IsCatalogTool(name string) bool {
	return name == CatalogSearchTool || name == CatalogDescribeTool || name == CatalogCallTool
}

// CheckToolConfig 实现 CFG-19 中需要注册表的部分与 CAT-01：注册表中不得有目录工具名；
// pinned 中的每一项都必须是注册表中存在的工具，且不能是目录工具名。
func CheckToolConfig(reg *protocol.Registry, pinned []string) error {
	for _, e := range reg.Tools() {
		if IsCatalogTool(e.Tool.Name) {
			return fmt.Errorf("mcp: tool name %q is reserved for the tool catalog", e.Tool.Name)
		}
	}
	for i, name := range pinned {
		if IsCatalogTool(name) {
			return fmt.Errorf("config: mcp.pinned_tools[%d]: %q is a catalog tool", i, name)
		}
		if _, ok := reg.Tool(name); !ok {
			return fmt.Errorf("config: mcp.pinned_tools[%d]: unknown tool %q", i, name)
		}
	}
	return nil
}

// catalogMode 实现 CAT-02：按客户端计算生效模式。visible 是该客户端按 TL-03 / TL-04 可见的真实工具。
func (s *Server) catalogMode(visible []map[string]any) bool {
	switch s.d.ToolMode {
	case ToolModeCatalog:
		return true
	case ToolModeAuto:
		th := s.d.AutoThreshold
		if th <= 0 {
			th = defaultAutoThreshold
		}
		return len(visible) > th
	default:
		return false
	}
}

// visibleTools 返回客户端 p 可见的真实工具（TL-01 ~ TL-04 的输出），以及生效模式是否为目录。
func (s *Server) visibleTools(p *auth.Principal) ([]map[string]any, bool) {
	visible := s.listTools(p)
	return visible, s.catalogMode(visible)
}

// toolsListResult 实现 tools/list：直接模式同 TL-01 ~ TL-04；目录模式为目录工具 + 可见的置顶工具（CAT-03）。
func (s *Server) toolsListResult(p *auth.Principal) []map[string]any {
	visible, catalog := s.visibleTools(p)
	if !catalog {
		return visible
	}
	out := catalogToolObjects()
	for _, t := range visible {
		if s.pinned[toolName(t)] {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return toolName(out[i]) < toolName(out[j]) })
	return out
}

func toolName(t map[string]any) string {
	n, _ := t["name"].(string)
	return n
}

// catalogToolObjects 返回三个目录工具的 MCP Tool 对象（CAT-04 ~ CAT-06）。
func catalogToolObjects() []map[string]any {
	return []map[string]any{
		{
			"name":        CatalogSearchTool,
			"title":       "Search gateway tools",
			"description": "Find tools available through this gateway (search APIs, document parsers, REST services, other models). Returns one line per tool: name and summary. Use this first, then gateway_describe_tool to see a tool's parameters, then gateway_call_tool to call it. An empty query lists every tool.",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Space-separated keywords; a tool matches when its name, title or description contains every keyword (case-insensitive). Empty lists all tools."},"limit":{"type":"integer","minimum":1,"maximum":100,"default":20,"description":"Maximum number of tools to return."},"offset":{"type":"integer","minimum":0,"default":0,"description":"Number of matching tools to skip, for paging."}},"additionalProperties":false}`),
			"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
		},
		{
			"name":        CatalogDescribeTool,
			"title":       "Describe a gateway tool",
			"description": "Return the full definition of one tool found with gateway_search_tools: description, input JSON Schema and whether it is read-only.",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Tool name returned by gateway_search_tools."}},"required":["name"],"additionalProperties":false}`),
			"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
		},
		{
			"name":        CatalogCallTool,
			"title":       "Call a gateway tool",
			"description": "Call one tool found with gateway_search_tools, passing arguments that match the input schema from gateway_describe_tool. The call is metered and billed like a direct call. Check the tool's readOnlyHint first: some tools change external state.",
			"inputSchema": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Tool name returned by gateway_search_tools."},"arguments":{"type":"object","description":"Arguments for the tool, matching its input schema."}},"required":["name"],"additionalProperties":false}`),
			"annotations": map[string]any{"readOnlyHint": false, "openWorldHint": true},
		},
	}
}

// catalogInstructions 是目录模式下 initialize 的 instructions 追加的句子（CAT-03）。
const catalogInstructions = " Use gateway_search_tools to find available tools, gateway_describe_tool to see a tool's parameters, and gateway_call_tool to call it."

// callCatalogTool 执行一个目录工具（CAT-04 ~ CAT-06、CAT-08），返回 tools/call 的结果对象。
// 只在目录模式下调用；visible 是该客户端可见的真实工具。
func (s *Server) callCatalogTool(r *http.Request, name string, args json.RawMessage, visible []map[string]any) map[string]any {
	switch name {
	case CatalogSearchTool:
		return s.searchTools(args, visible)
	case CatalogDescribeTool:
		var p struct {
			Name *string `json:"name"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Name == nil || *p.Name == "" {
			return toolResultObject(protocol.TextResult("invalid arguments: name must be a non-empty string", true))
		}
		t := findTool(visible, *p.Name)
		if t == nil {
			return toolResultObject(protocol.TextResult(unknownToolText(*p.Name), true))
		}
		b, err := json.Marshal(t)
		if err != nil {
			return toolResultObject(protocol.TextResult("internal error", true))
		}
		return toolResultObject(&protocol.ToolResult{Content: []protocol.ToolContent{{Type: "text", Text: string(b)}}, Structured: b})
	default: // CatalogCallTool
		var p struct {
			Name      *string         `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Name == nil || *p.Name == "" {
			return toolResultObject(protocol.TextResult("invalid arguments: name must be a non-empty string and arguments an object", true))
		}
		if a := strings.TrimSpace(string(p.Arguments)); a != "" && a != "null" && !strings.HasPrefix(a, "{") {
			return toolResultObject(protocol.TextResult("invalid arguments: arguments must be an object", true))
		}
		if IsCatalogTool(*p.Name) {
			return toolResultObject(protocol.TextResult("gateway_call_tool cannot call catalog tools", true))
		}
		if findTool(visible, *p.Name) == nil {
			return toolResultObject(protocol.TextResult(unknownToolText(*p.Name), true))
		}
		inner := p.Arguments
		if len(inner) == 0 || strings.TrimSpace(string(inner)) == "null" {
			inner = json.RawMessage("{}")
		}
		res, rpcErr := s.callTool(r, *p.Name, inner)
		if rpcErr != nil { // 可见性已检查，只在工具于检查后失效时出现
			return toolResultObject(protocol.TextResult(unknownToolText(*p.Name), true))
		}
		return toolResultObject(res)
	}
}

func unknownToolText(name string) string {
	return "unknown tool " + name + "; use gateway_search_tools"
}

func findTool(visible []map[string]any, name string) map[string]any {
	for _, t := range visible {
		if toolName(t) == name {
			return t
		}
	}
	return nil
}

// searchTools 实现 CAT-04。
func (s *Server) searchTools(args json.RawMessage, visible []map[string]any) map[string]any {
	var p struct {
		Query  *string `json:"query"`
		Limit  *int    `json:"limit"`
		Offset *int    `json:"offset"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolResultObject(protocol.TextResult("invalid arguments: query must be a string, limit and offset integers", true))
	}
	limit, offset := searchDefaultLimit, 0
	if p.Limit != nil {
		limit = *p.Limit
	}
	if p.Offset != nil {
		offset = *p.Offset
	}
	if limit < 1 || limit > searchMaxLimit {
		return toolResultObject(protocol.TextResult(fmt.Sprintf("invalid arguments: limit must be between 1 and %d", searchMaxLimit), true))
	}
	if offset < 0 {
		return toolResultObject(protocol.TextResult("invalid arguments: offset must be >= 0", true))
	}
	var words []string
	if p.Query != nil {
		words = strings.Fields(strings.ToLower(*p.Query))
	}

	var matched []map[string]any // visible 已按名称升序
	for _, t := range visible {
		title, _ := t["title"].(string)
		desc, _ := t["description"].(string)
		hay := strings.ToLower(toolName(t) + "\n" + title + "\n" + desc)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, t)
		}
	}
	total := len(matched)
	page := []map[string]any{}
	if offset < total {
		end := min(offset+limit, total)
		page = matched[offset:end]
	}

	items := make([]map[string]any, 0, len(page))
	var sb strings.Builder
	for _, t := range page {
		desc, _ := t["description"].(string)
		sum := summary(desc)
		item := map[string]any{"name": toolName(t), "summary": sum, "read_only": readOnly(t)}
		if title, _ := t["title"].(string); title != "" {
			item["title"] = title
		}
		items = append(items, item)
		fmt.Fprintf(&sb, "%s — %s\n", toolName(t), sum)
	}
	hasMore := offset+len(page) < total
	switch {
	case total == 0:
		sb.WriteString("no tools match\n")
	case hasMore:
		fmt.Fprintf(&sb, "(showing %d–%d of %d; pass offset=%d for more)\n", offset+1, offset+len(page), total, offset+len(page))
	}
	structured, err := json.Marshal(map[string]any{"tools": items, "total": total, "offset": offset, "has_more": hasMore})
	if err != nil {
		return toolResultObject(protocol.TextResult("internal error", true))
	}
	return toolResultObject(&protocol.ToolResult{
		Content:    []protocol.ToolContent{{Type: "text", Text: strings.TrimSuffix(sb.String(), "\n")}},
		Structured: structured,
	})
}

// summary 取描述的首行，超过 summaryMaxRunes 个字符时截断。
func summary(desc string) string {
	line, _, _ := strings.Cut(desc, "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > summaryMaxRunes {
		return string(r[:summaryMaxRunes]) + "…"
	}
	return line
}

func readOnly(t map[string]any) bool {
	a, _ := t["annotations"].(map[string]any)
	ro, _ := a["readOnlyHint"].(bool)
	return ro
}
