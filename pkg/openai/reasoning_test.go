package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageDecodesEitherReasoningField(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"reasoning_content", `{"role":"assistant","content":"hi","reasoning_content":"deepseek says it this way"}`, "deepseek says it this way"},
		{"reasoning", `{"role":"assistant","content":"hi","reasoning":"vllm says it that way"}`, "vllm says it that way"},
		{"both", `{"role":"assistant","content":"hi","reasoning_content":"preferred","reasoning":"ignored"}`, "preferred"},
		{"neither", `{"role":"assistant","content":"hi"}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var message ChatCompletionMessage
			if err := json.Unmarshal([]byte(tt.body), &message); err != nil {
				t.Fatal(err)
			}
			if message.ReasoningContent != tt.want {
				t.Fatalf("ReasoningContent = %q, want %q", message.ReasoningContent, tt.want)
			}
			if message.Content != "hi" {
				t.Fatalf("Content = %q, want the content still decoded", message.Content)
			}
		})
	}
}

// Requests keep the DeepSeek name, which vLLM and OpenRouter also accept, so
// the alias stays a decoding concession and does not change what is sent.
func TestMessageMarshalsReasoningContentOnly(t *testing.T) {
	raw, err := json.Marshal(ChatCompletionMessage{Role: RoleAssistant, Content: "hi", ReasoningContent: "thinking"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_content":"thinking"`) {
		t.Fatalf("payload = %s, want reasoning_content", raw)
	}
	if strings.Contains(string(raw), `"reasoning":`) {
		t.Fatalf("payload = %s, want no reasoning alias on the wire", raw)
	}
}

func TestStreamDeltaDecodesEitherReasoningField(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"reasoning_content", `{"reasoning_content":"deepseek chunk"}`, "deepseek chunk"},
		{"reasoning", `{"reasoning":"vllm chunk"}`, "vllm chunk"},
		{"both", `{"reasoning_content":"preferred","reasoning":"ignored"}`, "preferred"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var delta ChatCompletionStreamChoiceDelta
			if err := json.Unmarshal([]byte(tt.body), &delta); err != nil {
				t.Fatal(err)
			}
			if delta.ReasoningContent != tt.want {
				t.Fatalf("ReasoningContent = %q, want %q", delta.ReasoningContent, tt.want)
			}
		})
	}
}

// The delta carries the rest of a chunk, and gained a custom decoder, so the
// other fields are checked once to show the decoder did not shadow them.
func TestStreamDeltaDecodesRemainingFields(t *testing.T) {
	var delta ChatCompletionStreamChoiceDelta
	body := `{"role":"assistant","content":"text","reasoning":"why","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]}`
	if err := json.Unmarshal([]byte(body), &delta); err != nil {
		t.Fatal(err)
	}
	if delta.Role != "assistant" || delta.Content != "text" || delta.ReasoningContent != "why" {
		t.Fatalf("delta = %#v", delta)
	}
	if len(delta.ToolCalls) != 1 || delta.ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("tool calls = %#v", delta.ToolCalls)
	}
}
