package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func mapOf(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T: %v", v, v)
	}
	return m
}

type streamToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func parseChunk(t *testing.T, s string) []streamToolCall {
	t.Helper()
	payload := strings.TrimPrefix(s, "data: ")
	var out struct {
		Choices []struct {
			Delta struct {
				ToolCalls []streamToolCall `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("parse chunk %q: %v", s, err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expect 1 choice in %q", s)
	}
	return out.Choices[0].Delta.ToolCalls
}

func toolRoundtripMessages() []any {
	return []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "你是助手"},
		}},
		map[string]any{"role": "user", "content": "北京天气"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "晴 25 度"},
	}
}

func weatherTools() []any {
	return []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "get_weather", "description": "查天气",
		"parameters": map[string]any{"type": "object"},
	}}}
}

func TestAnthropicBuildBodyTools(t *testing.T) {
	ad := newAnthropicAdapter()
	req := map[string]any{
		"model": "claude-x", "messages": toolRoundtripMessages(),
		"tools": weatherTools(), "tool_choice": "required",
	}
	out, err := ad.buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := out["system"]; got != "你是助手" {
		t.Fatalf("system wrong: %v", got)
	}
	at := mapOf(t, out["tools"].([]any)[0])
	if at["name"] != "get_weather" || at["description"] != "查天气" {
		t.Fatalf("tool shape wrong: %v", at)
	}
	if _, ok := at["input_schema"]; !ok {
		t.Fatalf("input_schema missing: %v", at)
	}
	if tc := mapOf(t, out["tool_choice"]); tc["type"] != "any" {
		t.Fatalf("tool_choice required→any wrong: %v", tc)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expect 3 messages, got %d: %v", len(msgs), msgs)
	}
	tu := mapOf(t, mapOf(t, msgs[1])["content"].([]any)[0])
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "get_weather" {
		t.Fatalf("tool_use block wrong: %v", tu)
	}
	if input := mapOf(t, tu["input"]); input["city"] != "北京" {
		t.Fatalf("tool_use input not parsed as object: %v", tu["input"])
	}
	toolMsg := mapOf(t, msgs[2])
	if toolMsg["role"] != "user" {
		t.Fatalf("tool result must be user role: %v", toolMsg)
	}
	tr := mapOf(t, toolMsg["content"].([]any)[0])
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["content"] != "晴 25 度" {
		t.Fatalf("tool_result block wrong: %v", tr)
	}
}

func TestAnthropicToolChoiceVariants(t *testing.T) {
	ad := newAnthropicAdapter()
	base := func(choice any) map[string]any {
		return map[string]any{"model": "m", "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		}, "tools": weatherTools(), "tool_choice": choice}
	}
	out, err := ad.buildBody(base("auto"))
	if err != nil {
		t.Fatal(err)
	}
	if tc := mapOf(t, out["tool_choice"]); tc["type"] != "auto" {
		t.Fatalf("auto wrong: %v", tc)
	}
	out, _ = ad.buildBody(base("none"))
	if _, has := out["tools"]; has {
		t.Fatal("tool_choice none must drop tools")
	}
	out, _ = ad.buildBody(base(map[string]any{
		"type": "function", "function": map[string]any{"name": "get_weather"},
	}))
	if tc := mapOf(t, out["tool_choice"]); tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Fatalf("named tool_choice wrong: %v", tc)
	}
}

func TestAnthropicConvertBufferedToolUse(t *testing.T) {
	ad := newAnthropicAdapter()
	body := `{"id":"msg_1","model":"claude-x","stop_reason":"tool_use",
	 "content":[{"type":"text","text":"我先查一下"},
	            {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}],
	 "usage":{"input_tokens":10,"output_tokens":5}}`
	out, u, err := ad.convertBuffered([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	c := resp.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason wrong: %s", c.FinishReason)
	}
	if c.Message.Content != "我先查一下" {
		t.Fatalf("content wrong: %q", c.Message.Content)
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Fatalf("tool_call wrong: %+v", tc)
	}
	if tc.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("arguments wrong: %q", tc.Function.Arguments)
	}
	if u.prompt != 10 || u.completion != 5 {
		t.Fatalf("usage wrong: %+v", u)
	}
}

func TestAnthropicStreamToolCalls(t *testing.T) {
	ad := newAnthropicAdapter()
	start := ad.convertStreamChunk([]byte(
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`))
	if len(start) != 1 {
		t.Fatalf("expect 1 chunk, got %v", start)
	}
	tc0 := parseChunk(t, start[0])
	if len(tc0) != 1 || tc0[0].ID != "toolu_1" || tc0[0].Function.Name != "get_weather" ||
		tc0[0].Index == nil || *tc0[0].Index != 0 {
		t.Fatalf("start chunk wrong: %+v", tc0)
	}
	args := ""
	for _, part := range []string{`{"city"`, `:"北京"}`} {
		enc, _ := json.Marshal(part)
		chunks := ad.convertStreamChunk([]byte(
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + string(enc) + `}}`))
		if len(chunks) != 1 {
			t.Fatalf("expect 1 chunk for %q", part)
		}
		tcs := parseChunk(t, chunks[0])
		if len(tcs) != 1 || tcs[0].Index == nil || *tcs[0].Index != 0 {
			t.Fatalf("delta chunk wrong: %+v", tcs)
		}
		args += tcs[0].Function.Arguments
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("assembled arguments wrong: %q", args)
	}
}

func TestResponsesBuildBodyTools(t *testing.T) {
	ad := newResponsesAdapter()
	req := map[string]any{
		"model": "gpt-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "查天气"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "晴"},
		},
		"tools":      weatherTools(),
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
	}
	out, err := ad.buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	input := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("expect 3 input items, got %d: %v", len(input), input)
	}
	fc := mapOf(t, input[1])
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" ||
		fc["name"] != "get_weather" || fc["arguments"] != `{"city":"北京"}` {
		t.Fatalf("function_call item wrong: %v", fc)
	}
	fo := mapOf(t, input[2])
	if fo["type"] != "function_call_output" || fo["call_id"] != "call_1" || fo["output"] != "晴" {
		t.Fatalf("function_call_output item wrong: %v", fo)
	}
	ft := mapOf(t, out["tools"].([]any)[0])
	if ft["type"] != "function" || ft["name"] != "get_weather" || ft["description"] != "查天气" {
		t.Fatalf("flattened tool wrong: %v", ft)
	}
	if _, ok := ft["function"]; ok {
		t.Fatalf("nested function must be flattened: %v", ft)
	}
	if tc := mapOf(t, out["tool_choice"]); tc["type"] != "function" || tc["name"] != "get_weather" {
		t.Fatalf("tool_choice wrong: %v", tc)
	}
}

func TestResponsesConvertBufferedFunctionCall(t *testing.T) {
	ad := newResponsesAdapter()
	body := `{"id":"resp_1","model":"gpt-x","status":"completed",
	 "output":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"}],
	 "usage":{"input_tokens":7,"output_tokens":3}}`
	out, u, err := ad.convertBuffered([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	c := resp.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason wrong: %s", c.FinishReason)
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("tool_call wrong: %+v", tc)
	}
	if u.prompt != 7 || u.completion != 3 {
		t.Fatalf("usage wrong: %+v", u)
	}
}

func TestResponsesStreamToolCalls(t *testing.T) {
	ad := newResponsesAdapter()
	start := ad.convertStreamChunk([]byte(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":""}}`))
	if len(start) != 1 {
		t.Fatalf("expect 1 chunk, got %v", start)
	}
	tc0 := parseChunk(t, start[0])
	if len(tc0) != 1 || tc0[0].ID != "call_1" || tc0[0].Function.Name != "get_weather" ||
		tc0[0].Index == nil || *tc0[0].Index != 0 {
		t.Fatalf("start chunk wrong: %+v", tc0)
	}
	delta := ad.convertStreamChunk([]byte(
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":\"北京\"}"}`))
	if len(delta) != 1 {
		t.Fatalf("expect 1 chunk, got %v", delta)
	}
	tcs := parseChunk(t, delta[0])
	if len(tcs) != 1 || tcs[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("arguments delta wrong: %+v", tcs)
	}
}
