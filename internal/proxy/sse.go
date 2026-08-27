package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sseScan 在透传字节流的同时解析 SSE：累计 delta 文本、捕获最终 usage 块。
// 只读不改，透传给客户端的字节保持原样。
type sseScan struct {
	buf     bytes.Buffer
	text    strings.Builder
	usage   *sseUsage
	scanned bool
}

type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func newSSEScan() *sseScan { return &sseScan{} }

func (s *sseScan) Write(p []byte) {
	s.buf.Write(p)
	s.scanned = true
	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil {
			// ReadBytes 消费了缓冲，需把不完整行写回
			s.buf.Write(line)
			return
		}
		s.handleLine(trimEOL(line))
	}
}

func (s *sseScan) Finish() {
	if s.buf.Len() > 0 {
		s.handleLine(trimEOL(s.buf.Bytes()))
		s.buf.Reset()
	}
}

func trimEOL(b []byte) []byte {
	return []byte(strings.TrimRight(string(b), "\r\n"))
}

func (s *sseScan) handleLine(line []byte) {
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload := strings.TrimSpace(string(rest))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *sseUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	for _, c := range chunk.Choices {
		s.text.WriteString(c.Delta.Content)
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
}

func (s *sseScan) Usage() *sseUsage { return s.usage }

func (s *sseScan) Text() string { return s.text.String() }
