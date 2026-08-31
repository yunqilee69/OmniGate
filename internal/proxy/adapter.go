package proxy

import (
	"encoding/json"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/sashabaranov/go-openai"
)

// 协议适配层：客户端始终说 OpenAI chat/completions；adapter 负责出站请求构造与
// 入站响应/SSE 还原。wire 类型复用官方/社区 SDK（anthropic-sdk-go MIT、
// sashabaranov/go-openai Apache-2.0），此处只保留映射表（参考 one-api MIT 的
// relay/adaptor/anthropic 转换语义）。
type ProtocolAdapter interface {
	endpoint(baseURL string) string
	setHeaders(h map[string]string, apiKey string)
	buildBody(req map[string]any) (map[string]any, error)
	convertBuffered(body []byte) ([]byte, usageInfo, error)
	// convertStreamChunk 把上游一条 SSE data 载荷转为 0..n 条本端 "data: ..." 行
	convertStreamChunk(payload []byte) []string
	// streamFinal 流结束时补发（usage 块 + [DONE]）；openai 直通协议返回 nil
	streamFinal() []string
	streamUsage() *usageInfo
}

// -------------------- openai（直通） --------------------

type openaiAdapter struct{}

func (openaiAdapter) endpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}
func (openaiAdapter) setHeaders(h map[string]string, apiKey string) {
	h["Authorization"] = "Bearer " + apiKey
}
func (openaiAdapter) buildBody(req map[string]any) (map[string]any, error) {
	return req, nil
}
func (openaiAdapter) convertBuffered(body []byte) ([]byte, usageInfo, error) {
	return body, usageInfo{}, nil
}
func (openaiAdapter) convertStreamChunk([]byte) []string { return nil }
func (openaiAdapter) streamFinal() []string              { return nil }
func (openaiAdapter) streamUsage() *usageInfo            { return nil }

// -------------------- 通用工具 --------------------

func marshalChunk(chunk any) string {
	b, _ := json.Marshal(chunk)
	return "data: " + string(b)
}

func textChunk(content string) string {
	return marshalChunk(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Index: 0, Delta: openai.ChatCompletionStreamChoiceDelta{Content: content},
		}},
	})
}

// toolCallChunk 构造一条 delta.tool_calls 流式块（Index 指针指向块序号）
func toolCallChunk(index int, tc openai.ToolCall) string {
	idx := index
	tc.Index = &idx
	return marshalChunk(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Index: 0, Delta: openai.ChatCompletionStreamChoiceDelta{ToolCalls: []openai.ToolCall{tc}},
		}},
	})
}

// openAIText 抽取 OpenAI content（字符串或分块数组）中的纯文本
func openAIText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			if pm, ok := part.(map[string]any); ok && pm["type"] == "text" {
				if t, ok := pm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// parseJSONArgs 把 tool_call 的 arguments（JSON 字符串）解析为对象，坏值回退空对象
func parseJSONArgs(args string) any {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil || v == nil {
		return map[string]any{}
	}
	return v
}

// toolCallNameArgs 从 OpenAI tool_call 形状里取函数名与 arguments 字符串
func toolCallNameArgs(tm map[string]any) (name, args string) {
	fn, _ := tm["function"].(map[string]any)
	if fn != nil {
		name, _ = fn["name"].(string)
		args, _ = fn["arguments"].(string)
	}
	return
}

// anthropicFinishReason：one-api 同款 stop_reason → finish_reason 映射
func anthropicFinishReason(r anthropic.StopReason) openai.FinishReason {
	switch r {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return openai.FinishReasonStop
	case anthropic.StopReasonMaxTokens:
		return openai.FinishReasonLength
	case anthropic.StopReasonToolUse:
		return openai.FinishReasonToolCalls
	default:
		return openai.FinishReasonStop
	}
}

// -------------------- anthropic (/v1/messages) --------------------

type anthropicAdapter struct {
	inputTok, outputTok, cachedTok int
	seenUsage                      bool
}

func newAnthropicAdapter() *anthropicAdapter { return &anthropicAdapter{} }

func (*anthropicAdapter) endpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/messages"
}
func (*anthropicAdapter) setHeaders(h map[string]string, apiKey string) {
	h["x-api-key"] = apiKey
	h["anthropic-version"] = "2023-06-01"
}

func (*anthropicAdapter) buildBody(req map[string]any) (map[string]any, error) {
	out := map[string]any{"model": req["model"]}
	msgs, _ := req["messages"].([]any)
	converted := make([]any, 0, len(msgs))
	sysParts := []string{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch mm["role"] {
		case "system":
			if t := openAIText(mm["content"]); t != "" {
				sysParts = append(sysParts, t)
			}
		case "assistant":
			converted = append(converted, anthropicAssistantMsg(mm))
		case "tool":
			converted = append(converted, anthropicToolResultMsg(mm))
		default:
			converted = append(converted, map[string]any{"role": "user", "content": mm["content"]})
		}
	}
	out["messages"] = converted
	if len(sysParts) > 0 {
		out["system"] = strings.Join(sysParts, "\n\n")
	}
	if mt, ok := req["max_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	} else {
		out["max_tokens"] = 4096
	}
	for _, passthrough := range []string{"stream", "temperature", "top_p"} {
		if v, ok := req[passthrough]; ok {
			out[passthrough] = v
		}
	}
	if stop, ok := req["stop"]; ok {
		out["stop_sequences"] = stop
	}
	anthropicTools(req, out)
	return out, nil
}

// anthropicAssistantMsg：带 tool_calls 的 assistant 消息还原为 tool_use 内容块
func anthropicAssistantMsg(mm map[string]any) map[string]any {
	tcs, _ := mm["tool_calls"].([]any)
	if len(tcs) == 0 {
		return map[string]any{"role": "assistant", "content": mm["content"]}
	}
	blocks := []any{}
	if t := openAIText(mm["content"]); t != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": t})
	}
	for _, tc := range tcs {
		tm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		name, args := toolCallNameArgs(tm)
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": tm["id"], "name": name, "input": parseJSONArgs(args),
		})
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

// anthropicToolResultMsg：OpenAI tool 角色消息 → anthropic user 消息的 tool_result 块
func anthropicToolResultMsg(mm map[string]any) map[string]any {
	return map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "tool_result", "tool_use_id": mm["tool_call_id"], "content": openAIText(mm["content"]),
	}}}
}

// anthropicTools：OpenAI tools/tool_choice → anthropic 形状；tool_choice=none 时不带 tools（anthropic 无 none）
func anthropicTools(req, out map[string]any) {
	tools, _ := req["tools"].([]any)
	atools := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok || tm["type"] != "function" {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		if fn == nil {
			continue
		}
		at := map[string]any{"name": fn["name"]}
		if d, ok := fn["description"]; ok {
			at["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			at["input_schema"] = p
		}
		atools = append(atools, at)
	}
	if len(atools) == 0 {
		return
	}
	switch c := req["tool_choice"].(type) {
	case string:
		switch c {
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			return
		}
	case map[string]any:
		if c["type"] == "function" {
			if fn, ok := c["function"].(map[string]any); ok {
				if n, ok := fn["name"].(string); ok && n != "" {
					out["tool_choice"] = map[string]any{"type": "tool", "name": n}
				}
			}
		}
	}
	out["tools"] = atools
}

func (a *anthropicAdapter) convertBuffered(body []byte) ([]byte, usageInfo, error) {
	var msg anthropic.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, usageInfo{}, err
	}
	var sb strings.Builder
	var toolCalls []openai.ToolCall
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, openai.ToolCall{
				ID: block.ID, Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: block.Name, Arguments: args},
			})
			continue
		}
		sb.WriteString(block.AsText().Text)
	}
	assistant := openai.ChatCompletionMessage{Role: "assistant", Content: sb.String()}
	if len(toolCalls) > 0 {
		assistant.ToolCalls = toolCalls
	}
	resp := openai.ChatCompletionResponse{
		ID: msg.ID, Object: "chat.completion", Model: msg.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index: 0, FinishReason: anthropicFinishReason(msg.StopReason),
			Message: assistant,
		}},
		Usage: openai.Usage{
			PromptTokens:     int(msg.Usage.InputTokens),
			CompletionTokens: int(msg.Usage.OutputTokens),
			TotalTokens:      int(msg.Usage.InputTokens + msg.Usage.OutputTokens),
		},
	}
	conv, err := json.Marshal(resp)
	if err != nil {
		return nil, usageInfo{}, err
	}
	return conv, usageInfo{prompt: int(msg.Usage.InputTokens), completion: int(msg.Usage.OutputTokens), cached: int(msg.Usage.CacheReadInputTokens)}, nil
}

func (a *anthropicAdapter) convertStreamChunk(payload []byte) []string {
	var evt anthropic.MessageStreamEventUnion
	if err := json.Unmarshal(payload, &evt); err != nil {
		return nil
	}
	switch evt.Type {
	case "message_start":
		a.inputTok = int(evt.Message.Usage.InputTokens)
		a.cachedTok = int(evt.Message.Usage.CacheReadInputTokens)
		a.seenUsage = true
	case "message_delta":
		a.outputTok = int(evt.Usage.OutputTokens)
		if evt.Usage.InputTokens > 0 {
			a.inputTok = int(evt.Usage.InputTokens)
		}
		if evt.Usage.CacheReadInputTokens > 0 {
			a.cachedTok = int(evt.Usage.CacheReadInputTokens)
		}
		a.seenUsage = true
		return []string{marshalChunk(openai.ChatCompletionStreamResponse{
			Choices: []openai.ChatCompletionStreamChoice{{
				Index: 0, FinishReason: anthropicFinishReason(evt.Delta.StopReason),
			}},
		})}
	case "content_block_start":
		if evt.ContentBlock.Type == "tool_use" {
			return []string{toolCallChunk(int(evt.Index), openai.ToolCall{
				ID: evt.ContentBlock.ID, Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: evt.ContentBlock.Name},
			})}
		}
	case "content_block_delta":
		if evt.Delta.Type == "text_delta" && evt.Delta.Text != "" {
			return []string{textChunk(evt.Delta.Text)}
		}
		if evt.Delta.Type == "input_json_delta" && evt.Delta.PartialJSON != "" {
			return []string{toolCallChunk(int(evt.Index), openai.ToolCall{
				Function: openai.FunctionCall{Arguments: evt.Delta.PartialJSON},
			})}
		}
	}
	return nil
}

func (a *anthropicAdapter) streamFinal() []string {
	usage, _ := json.Marshal(map[string]any{
		"choices": []any{}, "usage": map[string]any{
			"prompt_tokens": a.inputTok, "completion_tokens": a.outputTok,
		},
	})
	return []string{"data: " + string(usage), "data: [DONE]"}
}

func (a *anthropicAdapter) streamUsage() *usageInfo {
	if !a.seenUsage {
		return nil
	}
	u := usageInfo{prompt: a.inputTok, completion: a.outputTok, cached: a.cachedTok}
	return &u
}

// -------------------- openai responses (/responses) --------------------

type responsesAdapter struct {
	usage openai.ResponseUsage
	seen  bool
}

func newResponsesAdapter() *responsesAdapter { return &responsesAdapter{} }

func (*responsesAdapter) endpoint(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/responses"
}
func (*responsesAdapter) setHeaders(h map[string]string, apiKey string) {
	h["Authorization"] = "Bearer " + apiKey
}

func (*responsesAdapter) buildBody(req map[string]any) (map[string]any, error) {
	out := map[string]any{"model": req["model"]}
	if msgs, ok := req["messages"].([]any); ok {
		out["input"] = responsesInput(msgs)
	}
	if mt, ok := req["max_tokens"].(float64); ok {
		out["max_output_tokens"] = int(mt)
	}
	for _, passthrough := range []string{"stream", "temperature", "top_p"} {
		if v, ok := req[passthrough]; ok {
			out[passthrough] = v
		}
	}
	responsesTools(req, out)
	return out, nil
}

// responsesInput：OpenAI chat messages → responses input 项；工具调用往返映射为
// function_call / function_call_output 项，普通消息直传（responses 易用格式）。
func responsesInput(msgs []any) []any {
	input := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
			if t := openAIText(mm["content"]); t != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": t})
			}
			for _, tc := range tcs {
				tm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				name, args := toolCallNameArgs(tm)
				input = append(input, map[string]any{
					"type": "function_call", "call_id": tm["id"], "name": name, "arguments": args,
				})
			}
			continue
		}
		if mm["role"] == "tool" {
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": mm["tool_call_id"], "output": openAIText(mm["content"]),
			})
			continue
		}
		input = append(input, mm)
	}
	return input
}

// responsesTools：嵌套 function 工具扁平化为 responses 形状；tool_choice 字符串直传、对象重映射
func responsesTools(req, out map[string]any) {
	tools, _ := req["tools"].([]any)
	flat := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok || tm["type"] != "function" {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		if fn == nil {
			continue
		}
		item := map[string]any{"type": "function", "name": fn["name"]}
		if d, ok := fn["description"]; ok {
			item["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			item["parameters"] = p
		}
		flat = append(flat, item)
	}
	if len(flat) > 0 {
		out["tools"] = flat
	}
	switch c := req["tool_choice"].(type) {
	case string:
		out["tool_choice"] = c
	case map[string]any:
		if c["type"] == "function" {
			if fn, ok := c["function"].(map[string]any); ok && fn["name"] != nil {
				out["tool_choice"] = map[string]any{"type": "function", "name": fn["name"]}
			}
		}
	}
}

func (r *responsesAdapter) convertBuffered(body []byte) ([]byte, usageInfo, error) {
	var src openai.CreateResponseResponse
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, usageInfo{}, err
	}
	content := src.OutputText
	if content == "" {
		for _, item := range src.Output {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "message" {
				continue
			}
			if parts, ok := m["content"].([]any); ok {
				for _, p := range parts {
					pm, ok := p.(map[string]any)
					if ok && pm["type"] == "output_text" {
						if t, ok := pm["text"].(string); ok {
							content += t
						}
					}
				}
			}
		}
	}
	finish := openai.FinishReasonStop
	if src.Status == openai.ResponseStatusIncomplete {
		finish = openai.FinishReasonLength
	}
	var toolCalls []openai.ToolCall
	for _, item := range src.Output {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "function_call" {
			continue
		}
		callID, _ := m["call_id"].(string)
		name, _ := m["name"].(string)
		args, _ := m["arguments"].(string)
		toolCalls = append(toolCalls, openai.ToolCall{
			ID: callID, Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: name, Arguments: args},
		})
	}
	if len(toolCalls) > 0 {
		finish = openai.FinishReasonToolCalls
	}
	u := usageInfo{}
	if src.Usage != nil {
		u = usageInfo{prompt: src.Usage.InputTokens, completion: src.Usage.OutputTokens}
	}
	resp := openai.ChatCompletionResponse{
		ID: src.ID, Object: "chat.completion", Model: src.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index: 0, FinishReason: finish,
			Message: openai.ChatCompletionMessage{Role: "assistant", Content: content, ToolCalls: toolCalls},
		}},
		Usage: openai.Usage{
			PromptTokens: u.prompt, CompletionTokens: u.completion,
			TotalTokens: u.prompt + u.completion,
		},
	}
	conv, err := json.Marshal(resp)
	if err != nil {
		return nil, usageInfo{}, err
	}
	return conv, u, nil
}

func (r *responsesAdapter) convertStreamChunk(payload []byte) []string {
	var evt openai.ResponseStreamEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		return nil
	}
	switch evt.Type {
	case openai.ResponseStreamEventOutputTextDelta:
		if evt.Delta != "" {
			return []string{textChunk(evt.Delta)}
		}
	case openai.ResponseStreamEventOutputItemAdded:
		if evt.Item != nil && evt.Item.Type == "function_call" {
			return []string{toolCallChunk(evt.OutputIndex, openai.ToolCall{
				ID: evt.Item.CallID, Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: evt.Item.Name},
			})}
		}
	case openai.ResponseStreamEventFunctionArgumentsDelta:
		if evt.Delta != "" {
			return []string{toolCallChunk(evt.OutputIndex, openai.ToolCall{
				Function: openai.FunctionCall{Arguments: evt.Delta},
			})}
		}
	case openai.ResponseStreamEventCompleted, openai.ResponseStreamEventIncomplete:
		if evt.Response != nil && evt.Response.Usage != nil {
			r.usage = *evt.Response.Usage
			r.seen = true
			finish := openai.FinishReasonStop
			if evt.Type == openai.ResponseStreamEventIncomplete {
				finish = openai.FinishReasonLength
			}
			return []string{marshalChunk(openai.ChatCompletionStreamResponse{
				Choices: []openai.ChatCompletionStreamChoice{{Index: 0, FinishReason: finish}},
			})}
		}
	}
	return nil
}

func (r *responsesAdapter) streamFinal() []string {
	usage, _ := json.Marshal(map[string]any{
		"choices": []any{}, "usage": map[string]any{
			"prompt_tokens": r.usage.InputTokens, "completion_tokens": r.usage.OutputTokens,
		},
	})
	return []string{"data: " + string(usage), "data: [DONE]"}
}

func (r *responsesAdapter) streamUsage() *usageInfo {
	if !r.seen {
		return nil
	}
	u := usageInfo{prompt: r.usage.InputTokens, completion: r.usage.OutputTokens}
	return &u
}

func AdapterFor(protocol string) ProtocolAdapter {
	switch protocol {
	case "anthropic":
		return newAnthropicAdapter()
	case "responses":
		return newResponsesAdapter()
	default:
		return openaiAdapter{}
	}
}

// sseSplitter 把字节流切成完整的 "data:..." 载荷（跨 chunk 缓冲）。
type sseSplitter struct {
	buf strings.Builder
}

func (s *sseSplitter) write(p []byte) [][]byte {
	s.buf.Write(p)
	var out [][]byte
	for {
		rest := s.buf.String()
		idx := strings.IndexByte(rest, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(rest[:idx], "\r")
		s.buf.Reset()
		s.buf.WriteString(rest[idx+1:])
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			payload := strings.TrimSpace(data)
			if payload != "" {
				out = append(out, []byte(payload))
			}
		}
	}
	return out
}

func (s *sseSplitter) flush() [][]byte {
	rest := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	if rest == "" {
		return nil
	}
	if data, ok := strings.CutPrefix(rest, "data:"); ok {
		payload := strings.TrimSpace(data)
		if payload != "" {
			return [][]byte{[]byte(payload)}
		}
	}
	return nil
}
