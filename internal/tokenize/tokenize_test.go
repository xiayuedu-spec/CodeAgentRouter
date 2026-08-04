package tokenize

import "testing"

func TestEstimateText(t *testing.T) {
	if got := EstimateText("hello world"); got != 3 {
		t.Fatalf("EstimateText(hello world) = %d, want 3", got)
	}
	if got := EstimateText("你好世界"); got != 4 {
		t.Fatalf("EstimateText(CJK) = %d, want 4", got)
	}
}

func TestEstimateChatPayload(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"max_tokens": 100,
	}
	prompt, completion := EstimateChatPayload(payload)
	if prompt != 5 { // 4 overhead + 1 text
		t.Fatalf("prompt = %d, want 5", prompt)
	}
	if completion != 100 {
		t.Fatalf("completion = %d, want 100", completion)
	}
}

func TestEstimateCompletionCap(t *testing.T) {
	if got := EstimateCompletionTokens(999999, 4); got != MaxCompletionEstimate {
		t.Fatalf("capped completion = %d, want %d", got, MaxCompletionEstimate)
	}
}
