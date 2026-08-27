package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithMessages(msgs any) map[string]any {
	return map[string]any{"messages": msgs}
}

func TestSessionKeyHeaderWins(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Session-ID", " sess-1 ")
	req := reqWithMessages([]any{
		map[string]any{"role": "user", "content": "hello"},
	})
	if got := sessionKey(r, req, "X-Session-ID"); got != "h:sess-1" {
		t.Fatalf("header must win and be trimmed: %q", got)
	}
	if got := sessionKey(r, req, ""); got == "" || got[:2] != "m:" {
		t.Fatalf("empty header name must fall back to prefix hash: %q", got)
	}
}

func TestMessagePrefixKeyStableAcrossTurns(t *testing.T) {
	turn1 := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "hi"},
	})
	turn2 := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello!"},
		map[string]any{"role": "user", "content": "more"},
	})
	turn3 := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello!"},
		map[string]any{"role": "user", "content": "more"},
		map[string]any{"role": "assistant", "content": "sure"},
		map[string]any{"role": "user", "content": "even more"},
	})
	k1, k2, k3 := messagePrefixKey(turn1), messagePrefixKey(turn2), messagePrefixKey(turn3)
	if k1 == "" || k1 != k2 || k2 != k3 {
		t.Fatalf("prefix key must be stable across turns: %q %q %q", k1, k2, k3)
	}

	// 首条 assistant 之前的前缀不同 → 键不同
	other := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "different opener"},
	})
	if messagePrefixKey(other) == k1 {
		t.Fatal("different prefix must hash differently")
	}

	// assistant 之后的消息不影响键（前缀切面）
	turn2AltAssistant := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "totally different reply"},
	})
	if messagePrefixKey(turn2AltAssistant) != k1 {
		t.Fatal("post-prefix content must not affect key")
	}
}

func TestMessagePrefixKeyEatsWholeFirstTurn(t *testing.T) {
	// few-shot：首条 assistant 之前有多条 user/tool 消息，全部进入指纹
	fewShot := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "example q"},
		map[string]any{"role": "user", "content": "example q2"},
		map[string]any{"role": "assistant", "content": "example a"},
		map[string]any{"role": "user", "content": "real q"},
	})
	trimmed := reqWithMessages([]any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "example q"},
		map[string]any{"role": "assistant", "content": "example a"},
		map[string]any{"role": "user", "content": "real q"},
	})
	if messagePrefixKey(fewShot) == messagePrefixKey(trimmed) {
		t.Fatal("extra pre-assistant message must change the key")
	}
}

func TestMessagePrefixKeyDegenerate(t *testing.T) {
	if got := messagePrefixKey(map[string]any{}); got != "" {
		t.Fatalf("no messages: %q", got)
	}
	if got := messagePrefixKey(reqWithMessages([]any{})); got != "" {
		t.Fatalf("empty messages: %q", got)
	}
	if got := messagePrefixKey(reqWithMessages([]any{
		map[string]any{"role": "assistant", "content": "first"},
	})); got != "" {
		t.Fatalf("assistant-first prefix: %q", got)
	}
	// 多模态 content（数组）可稳定序列化
	mm := reqWithMessages([]any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
		}},
	})
	if got := messagePrefixKey(mm); got == "" {
		t.Fatal("array content must produce a key")
	}
}
