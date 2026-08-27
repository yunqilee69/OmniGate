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
	inputTok, outputTok int
	seenUsage           bool
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
		role, _ := mm["role"].(string)
		if role == "system" {
			if c, ok := mm["content"].(string); ok {
				sysParts = append(sysParts, c)
			}
			continue
		}
		if role != "user" && role != "assistant" {
			role = "user"
		}
		converted = append(converted, map[string]any{"role": role, "content": mm["content"]})
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
	return out, nil
}

func (a *anthropicAdapter) convertBuffered(body []byte) ([]byte, usageInfo, error) {
	var msg anthropic.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, usageInfo{}, err
	}
	var sb strings.Builder
	for _, block := range msg.Content {
		sb.WriteString(block.AsText().Text)
	}
	resp := openai.ChatCompletionResponse{
		ID: msg.ID, Object: "chat.completion", Model: msg.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index: 0, FinishReason: anthropicFinishReason(msg.StopReason),
			Message: openai.ChatCompletionMessage{Role: "assistant", Content: sb.String()},
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
	return conv, usageInfo{prompt: int(msg.Usage.InputTokens), completion: int(msg.Usage.OutputTokens)}, nil
}

func (a *anthropicAdapter) convertStreamChunk(payload []byte) []string {
	var evt anthropic.MessageStreamEventUnion
	if err := json.Unmarshal(payload, &evt); err != nil {
		return nil
	}
	switch evt.Type {
	case "message_start":
		a.inputTok = int(evt.Message.Usage.InputTokens)
		a.seenUsage = true
	case "message_delta":
		a.outputTok = int(evt.Usage.OutputTokens)
		if evt.Usage.InputTokens > 0 {
			a.inputTok = int(evt.Usage.InputTokens)
		}
		a.seenUsage = true
		return []string{marshalChunk(openai.ChatCompletionStreamResponse{
			Choices: []openai.ChatCompletionStreamChoice{{
				Index: 0, FinishReason: anthropicFinishReason(evt.Delta.StopReason),
			}},
		})}
	case "content_block_delta":
		if evt.Delta.Type == "text_delta" && evt.Delta.Text != "" {
			return []string{textChunk(evt.Delta.Text)}
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
	u := usageInfo{prompt: a.inputTok, completion: a.outputTok}
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
		out["input"] = msgs
	}
	if mt, ok := req["max_tokens"].(float64); ok {
		out["max_output_tokens"] = int(mt)
	}
	for _, passthrough := range []string{"stream", "temperature", "top_p"} {
		if v, ok := req[passthrough]; ok {
			out[passthrough] = v
		}
	}
	return out, nil
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
	u := usageInfo{}
	if src.Usage != nil {
		u = usageInfo{prompt: src.Usage.InputTokens, completion: src.Usage.OutputTokens}
	}
	resp := openai.ChatCompletionResponse{
		ID: src.ID, Object: "chat.completion", Model: src.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index: 0, FinishReason: finish,
			Message: openai.ChatCompletionMessage{Role: "assistant", Content: content},
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
