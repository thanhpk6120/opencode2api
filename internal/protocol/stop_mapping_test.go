package protocol

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestStopMappersAcceptKebabCase(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"chatStop tool-calls", chatStop("tool-calls"), "tool_calls"},
		{"chatStop tool_call", chatStop("tool_call"), "tool_calls"},
		{"chatStop tool_use", chatStop("tool_use"), "tool_calls"},
		{"anthropicStop tool-calls", anthropicStop("tool-calls"), "tool_use"},
		{"canonicalChatStop tool-calls", canonicalChatStop("tool-calls"), "tool_calls"},
		{"canonicalAnthropicStop tool-calls", canonicalAnthropicStop("tool-calls"), "tool_calls"},
	}
	for _, test := range cases {
		if test.got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, test.got, test.want)
		}
	}
	if !isToolStop("tool-calls") || !isToolStop("tool_call") {
		t.Error("isToolStop rejects kebab-case tool-call finishes")
	}
}

// A streamed reasoning delta that is only whitespace must survive parsing;
// jsonutil.FirstString trims and drops it, which glued reasoning tokens
// together downstream.
func TestChatReasoningDeltaKeepsWhitespace(t *testing.T) {
	parser := &bridgeStreamParser{
		protocol:          Chat,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	events, err := parser.Parse("", `{"choices":[{"delta":{"reasoning_content":" "}}]}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "reasoning" || events[0].Text != " " {
		t.Fatalf("whitespace reasoning delta dropped: %#v", events)
	}
}

// A Responses upstream may stream full reasoning text, not only summaries.
func TestResponsesReasoningTextDeltaStreams(t *testing.T) {
	parser := &bridgeStreamParser{
		protocol:          Responses,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	events, err := parser.Parse("response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","delta":"step"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "reasoning" || !strings.Contains(events[0].Text, "step") {
		t.Fatalf("reasoning_text.delta not streamed: %#v", events)
	}
}

// An upstream chat stream that terminates with the hyphenated
// "finish_reason":"tool-calls" must surface as "tool_calls" downstream, both
// in the collapsed (non-streaming) path and in the transcode (streaming) path.
func TestCollapseStreamNormalizesHyphenatedToolCalls(t *testing.T) {
	upstream := "" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool-calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	body, err := CollapseStream(strings.NewReader(upstream), Chat, "m")
	if err != nil {
		t.Fatalf("CollapseStream: %v", err)
	}
	if !strings.Contains(string(body), `"finish_reason":"stop"`) {
		t.Fatalf("expected phantom tool-calls finish demoted to stop, got %s", body)
	}
}

// The Chat streaming branch must flush buffered tool arguments at Finish and
// promote an empty arguments string to "{}", matching the non-streaming
// encoder; otherwise strict clients reject the tool call.
func TestChatFinishFlushesToolArguments(t *testing.T) {
	var buf bytes.Buffer
	writer := &flushRecorder{Writer: &buf}
	emitter := newBridgeStreamEmitter(writer, writer, Chat, "m")

	if err := emitter.Emit(bridgeStreamEvent{Kind: "tool_start", ToolKey: "0", ToolID: "call_1", ToolName: "read"}); err != nil {
		t.Fatalf("tool_start: %v", err)
	}
	if err := emitter.Emit(bridgeStreamEvent{Kind: "tool_delta", ToolKey: "0", Text: `{"file":"a"}`}); err != nil {
		t.Fatalf("tool_delta: %v", err)
	}
	// A second, argument-less tool must still receive "{}".
	if err := emitter.Emit(bridgeStreamEvent{Kind: "tool_start", ToolKey: "1", ToolID: "call_2", ToolName: "list"}); err != nil {
		t.Fatalf("second tool_start: %v", err)
	}
	if err := emitter.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("finish chunk missing tool_calls: %s", out)
	}
	if !strings.Contains(out, `\"file\":\"a\"`) {
		t.Fatalf("streamed arguments lost: %s", out)
	}
	if !strings.Contains(out, `"arguments":"{}"`) {
		t.Fatalf("empty tool arguments not promoted to {}: %s", out)
	}
}

type flushRecorder struct {
	io.Writer
}

func (f *flushRecorder) Flush() {}
