package tokenize

import (
	"encoding/json"
	"math"
	"unicode"
)

// MaxCompletionEstimate caps the completion reservation so a request with a
// huge max_tokens value cannot exhaust the pool by itself.
const MaxCompletionEstimate = 16384

// EstimateText is a lightweight tiktoken-style fallback: CJK characters are
// roughly one token each and everything else is about four characters per
// token. Upstream usage, when present, always takes precedence.
func EstimateText(text string) int64 {
	tokens := 0.0
	for _, r := range text {
		if isCJK(r) {
			tokens++
		} else {
			tokens += 0.25
		}
	}
	return int64(math.Ceil(tokens))
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// EstimateChatPrompt estimates prompt tokens from a chat completions body.
func EstimateChatPrompt(messages []map[string]any) int64 {
	var total int64
	for _, m := range messages {
		total += 4 // per-message structural overhead
		switch v := m["content"].(type) {
		case string:
			total += EstimateText(v)
		case []any:
			for _, part := range v {
				if pm, ok := part.(map[string]any); ok {
					if s, ok := pm["text"].(string); ok {
						total += EstimateText(s)
					}
				}
			}
		}
	}
	return total
}

// EstimateCompletionTokens estimates the completion reservation from
// max_tokens and n, defaulting to 4096 when max_tokens is absent.
func EstimateCompletionTokens(maxTokens, n int) int64 {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	if n <= 0 {
		n = 1
	}
	total := int64(maxTokens) * int64(n)
	if total > MaxCompletionEstimate {
		return MaxCompletionEstimate
	}
	return total
}

// EstimatePromptFromAny estimates prompt tokens for completions/embeddings
// where the prompt/input may be a string or an array of strings.
func EstimatePromptFromAny(v any) int64 {
	switch x := v.(type) {
	case string:
		return EstimateText(x)
	case []any:
		var total int64
		for _, item := range x {
			if s, ok := item.(string); ok {
				total += EstimateText(s)
			}
		}
		return total
	case []string:
		var total int64
		for _, s := range x {
			total += EstimateText(s)
		}
		return total
	}
	return 0
}

// EstimateChatPayload returns prompt + completion estimate for chat bodies.
func EstimateChatPayload(payload map[string]any) (prompt, completion int64) {
	raw, _ := payload["messages"].([]any)
	messages := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			messages = append(messages, mm)
		}
	}
	prompt = EstimateChatPrompt(messages)
	completion = EstimateCompletionTokens(intNumber(payload["max_tokens"]), intNumber(payload["n"]))
	return prompt, completion
}

// EstimateCompletionPayload returns prompt + completion estimate for legacy
// completions bodies.
func EstimateCompletionPayload(payload map[string]any) (prompt, completion int64) {
	prompt = EstimatePromptFromAny(payload["prompt"])
	completion = EstimateCompletionTokens(intNumber(payload["max_tokens"]), intNumber(payload["n"]))
	return prompt, completion
}

// EstimateEmbeddingPayload estimates prompt tokens for embeddings bodies.
func EstimateEmbeddingPayload(payload map[string]any) int64 {
	return EstimatePromptFromAny(payload["input"])
}

func intNumber(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	}
	return 0
}
