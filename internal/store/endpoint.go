package store

// EndpointFamilies 与前端路由页 tab 对齐：对话 / 向量 / 重排 / 生图 / MCP。
var EndpointFamilies = map[string][]string{
	"chat":      {"completions", "messages", "responses"},
	"embedding": {"embedding"},
	"rerank":    {"rerank"},
	"image":     {"image"},
	"mcp":       {"mcp"},
}

// EndpointsForFilter 把日志/统计的 endpoint 查询值展开为物理端点。
// 传入 tab key（chat）返回该族全部端点；传入具体端点则原样返回。
func EndpointsForFilter(v string) []string {
	if v == "" {
		return nil
	}
	if eps, ok := EndpointFamilies[v]; ok {
		return append([]string(nil), eps...)
	}
	return []string{v}
}
