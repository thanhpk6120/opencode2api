package protocol

import (
	"testing"
)

func mustDecode(t *testing.T, body map[string]any) bridgeRequest {
	t.Helper()
	request, err := decodeBridgeRequest(Chat, body)
	if err != nil {
		t.Fatalf("decodeBridgeRequest: %v", err)
	}
	return request
}

func toolHistoryRequest(messages ...map[string]any) map[string]any {
	items := make([]any, 0, len(messages))
	for _, message := range messages {
		items = append(items, message)
	}
	return map[string]any{"model": "m", "messages": items}
}

// Clients prune their history. A tool result whose assistant tool call was
// dropped must not fail the whole request: the upstream gate would reject it,
// and there is nothing to attach it to.
func TestOrphanToolResultIsDropped(t *testing.T) {
	body := toolHistoryRequest(
		map[string]any{"role": "user", "content": "read the file"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "one"},
		map[string]any{"role": "assistant", "content": "done"},
		map[string]any{"role": "user", "content": "again"},
		// The assistant turn that owned this call is gone.
		map[string]any{"role": "tool", "tool_call_id": "<path>", "content": "orphan"},
	)
	out, err := encodeChatRequest(mustDecode(t, body))
	if err != nil {
		t.Fatalf("orphan tool result rejected: %v", err)
	}
	for _, raw := range out["messages"].([]any) {
		message := raw.(map[string]any)
		if id, _ := message["tool_call_id"].(string); id == "<path>" {
			t.Fatal("orphan tool result was forwarded to upstream")
		}
	}
}

// A conversation that ends on an unanswered tool call still has to produce a
// valid payload: every tool call needs a matching result message.
func TestMissingToolResultIsSynthesized(t *testing.T) {
	body := toolHistoryRequest(
		map[string]any{"role": "user", "content": "run it"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{}"}},
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "one"},
	)
	out, err := encodeChatRequest(mustDecode(t, body))
	if err != nil {
		t.Fatalf("missing tool result rejected: %v", err)
	}
	seen := map[string]bool{}
	for _, raw := range out["messages"].([]any) {
		message := raw.(map[string]any)
		if message["role"] == "tool" {
			seen[message["tool_call_id"].(string)] = true
		}
	}
	if !seen["call_1"] || !seen["call_2"] {
		t.Fatalf("tool results not closed: %#v", seen)
	}
}
