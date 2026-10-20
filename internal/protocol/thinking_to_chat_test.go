package protocol

import "testing"

// prepareChatBody mimics how the gateway prepares a same-protocol Chat body.
func prepareChatBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	if _, ok := body["model"]; !ok {
		body["model"] = "m"
	}
	if _, ok := body["messages"]; !ok {
		body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
	}
	out, err := PrepareRequest(Chat, Chat, body, "https://example.test")
	if err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	return out
}

// A client that sends an Anthropic-shaped thinking block to a Chat endpoint used
// to have it forwarded verbatim. Strict upstream gates then reject the whole
// request with `json: unknown field "thinking"`.
func TestThinkingIsTranslatedForChatDestination(t *testing.T) {
	cases := []struct {
		name   string
		body   map[string]any
		effort any // nil means "no reasoning_effort expected"
	}{
		{
			name:   "effort inside thinking",
			body:   map[string]any{"thinking": map[string]any{"type": "enabled", "effort": "high"}},
			effort: "high",
		},
		{
			name:   "budget only",
			body:   map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(8192)}},
			effort: "high",
		},
		{
			name:   "disabled reasoning drops the field",
			body:   map[string]any{"thinking": map[string]any{"type": "disabled"}},
			effort: nil,
		},
		{
			name:   "client already named an effort",
			body:   map[string]any{"reasoning_effort": "low", "thinking": map[string]any{"effort": "high"}},
			effort: "low",
		},
	}
	for _, test := range cases {
		out := prepareChatBody(t, test.body)
		if _, leaked := out["thinking"]; leaked {
			t.Fatalf("%s: thinking survived to a Chat destination", test.name)
		}
		got, has := out["reasoning_effort"]
		if test.effort == nil {
			if has {
				t.Fatalf("%s: unexpected reasoning_effort %v", test.name, got)
			}
			continue
		}
		if got != test.effort {
			t.Fatalf("%s: reasoning_effort = %v, want %v", test.name, got, test.effort)
		}
	}
}

// The rewrite is Chat-only: an Anthropic destination keeps its native shape.
func TestThinkingIsKeptForAnthropicDestination(t *testing.T) {
	body := map[string]any{
		"model": "claude-x", "max_tokens": float64(4096),
		"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(1024)},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := PrepareRequest(Anthropic, Anthropic, body, "https://example.test")
	if err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	if _, ok := out["thinking"]; !ok {
		t.Fatal("thinking was dropped on an Anthropic destination")
	}
}
