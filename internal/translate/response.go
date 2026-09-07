package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Stop reasons. Neither format's vocabulary is a superset of the other's, so
// each unmapped value falls back to the one that means "the model finished",
// which is the reading that leaves a client's control flow intact.
func stopReasonToAnthropic(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		// Anthropic has no refusal reason on a completed message. Reporting
		// end_turn is honest about what happened to the stream: it stopped, and
		// the content the upstream did return is what there is.
		return "end_turn"
	case "":
		return ""
	}
	return "end_turn"
}

func stopReasonToOpenAI(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use", "pause_turn":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	}
	return "stop"
}

// usageToAnthropic recomputes an OpenAI usage object under Anthropic's
// convention.
//
// This is the arithmetic the whole feature turns on. OpenAI's prompt_tokens
// includes every cached token; Anthropic's input_tokens excludes them. Copying
// the number across would report each cached token twice — once at the full
// input rate inside input_tokens, once at the cache rate beside it — and the
// gateway's own pricing would then bill a translated reply more than the
// invoice says. So the cached parts are carved out of the total rather than
// added beside it, and each part is clamped against the total the provider
// itself reported, so an upstream's arithmetic error cannot mint savings or
// drive input negative.
func usageToAnthropic(u *openAIUsage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	total := nonNegative(u.PromptTokens)
	read := u.cacheRead()
	written := u.cacheWritten()
	if read > total {
		read = total
	}
	if written > total-read {
		written = total - read
	}
	out := anthropicUsage{
		InputTokens:              total - read - written,
		OutputTokens:             nonNegative(u.CompletionTokens),
		CacheReadInputTokens:     read,
		CacheCreationInputTokens: written,
	}
	if d := u.PromptTokensDetails; d != nil && written > 0 {
		out.CacheCreation = d.CacheCreation
	}
	return out
}

// usageToOpenAI is the same arithmetic run backwards: Anthropic's three
// independent counters are summed into the one total OpenAI reports, and the
// cached parts are restated as the breakdown that sits beside it.
func usageToOpenAI(u anthropicUsage) *openAIUsage {
	input := nonNegative(u.InputTokens)
	read := nonNegative(u.CacheReadInputTokens)
	written := nonNegative(u.CacheCreationInputTokens)
	prompt := input + read + written
	out := &openAIUsage{
		PromptTokens:     prompt,
		CompletionTokens: nonNegative(u.OutputTokens),
		TotalTokens:      prompt + nonNegative(u.OutputTokens),
	}
	if read > 0 || written > 0 {
		out.PromptTokensDetails = &openAIPromptDetails{
			CachedTokens:             read,
			CacheReadInputTokens:     read,
			CacheCreationInputTokens: written,
			CacheCreation:            u.CacheCreation,
		}
	}
	return out
}

// openAIResponseToAnthropic rewrites a completed Chat Completions reply as a
// Messages API reply.
//
// Only the first choice is carried. The Messages API has no way to express
// several alternative completions of one prompt, so a reply to an n > 1 request
// arrives with the alternatives dropped rather than concatenated into one
// answer — which is why n is refused on the way in and listed in Loss.
func openAIResponseToAnthropic(body []byte) ([]byte, error) {
	var in openAIResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode openai response: %w", err)
	}

	out := anthropicResponse{
		ID:      messageID(in.ID),
		Type:    "message",
		Role:    "assistant",
		Model:   in.Model,
		Content: []anthropicBlock{},
		Usage:   usageToAnthropic(in.Usage),
	}

	if len(in.Choices) > 0 {
		choice := in.Choices[0]
		out.StopReason = stopReasonToAnthropic(choice.FinishReason)
		if msg := choice.Message; msg != nil {
			if msg.ReasoningContent != "" {
				// Emitted as a thinking block with no signature. Anthropic
				// would refuse such a block on the way back in, and a client
				// replaying this turn will have it dropped by the request
				// translator for exactly that reason — but a client that only
				// displays reasoning gets to display it.
				out.Content = append(out.Content, anthropicBlock{
					Type:     "thinking",
					Thinking: msg.ReasoningContent,
				})
			}
			if text := openAIContentText(msg.Content); text != "" {
				out.Content = append(out.Content, anthropicBlock{Type: "text", Text: text})
			}
			for _, call := range msg.ToolCalls {
				input := json.RawMessage(call.Function.Arguments)
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage(`{}`)
				}
				out.Content = append(out.Content, anthropicBlock{
					Type:  "tool_use",
					ID:    toolUseID(call.ID),
					Name:  call.Function.Name,
					Input: input,
				})
			}
		}
	}
	if out.StopReason == "" {
		out.StopReason = "end_turn"
	}
	return json.Marshal(out)
}

// anthropicResponseToOpenAI rewrites a completed Messages API reply as a Chat
// Completions reply.
func anthropicResponseToOpenAI(body []byte) ([]byte, error) {
	var in anthropicResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}

	msg := openAIMessage{Role: "assistant"}
	var text strings.Builder
	for _, block := range in.Content {
		switch block.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(block.Text)
		case "thinking":
			if block.Thinking != "" {
				msg.ReasoningContent = block.Thinking
			}
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
				ID:       block.ID,
				Type:     "function",
				Function: openAIToolCallFunction{Name: block.Name, Arguments: args},
			})
		}
	}
	// content is required by the schema and null is its value for a turn that
	// only called tools, which is what OpenAI itself returns there.
	content, err := json.Marshal(text.String())
	if err != nil {
		return nil, err
	}
	if text.Len() > 0 || len(msg.ToolCalls) == 0 {
		msg.Content = content
	}

	out := openAIResponse{
		ID:     completionID(in.ID),
		Object: "chat.completion",
		Model:  in.Model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      &msg,
			FinishReason: stopReasonToOpenAI(in.StopReason),
		}},
		Usage: usageToOpenAI(in.Usage),
	}
	if out.Choices[0].FinishReason == "" {
		out.Choices[0].FinishReason = "stop"
	}
	return json.Marshal(out)
}

// Identifier prefixes. A client that pattern-matches on an id — and Claude Code
// does, when it correlates a reply with the turn that produced it — sees the
// shape its own format uses rather than the upstream's.
func messageID(id string) string {
	if id == "" {
		return "msg_translated"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func completionID(id string) string {
	if id == "" {
		return "chatcmpl-translated"
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	return "chatcmpl-" + id
}

// toolUseID gives a tool call the identifier shape Anthropic clients expect.
// The value has to survive the round trip unchanged in substance, because the
// client sends it back as tool_use_id and the upstream matches on it, so the
// prefix is added rather than the identifier replaced.
func toolUseID(id string) string {
	if id == "" {
		return "toolu_translated"
	}
	if strings.HasPrefix(id, "toolu_") {
		return id
	}
	return "toolu_" + id
}

// openAIErrorToAnthropic rewraps an OpenAI error envelope in Anthropic's,
// keeping the upstream's own message text.
func openAIErrorToAnthropic(body []byte) []byte {
	var in struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Error == nil {
		return body
	}
	kind := in.Error.Type
	if kind == "" {
		kind = "api_error"
	}
	out, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    kind,
			"message": in.Error.Message,
		},
	})
	if err != nil {
		return body
	}
	return out
}

// anthropicErrorToOpenAI is the same rewrap in the other direction.
func anthropicErrorToOpenAI(body []byte) []byte {
	var in struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Error == nil {
		return body
	}
	kind := in.Error.Type
	if kind == "" {
		kind = "api_error"
	}
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    kind,
			"message": in.Error.Message,
			"code":    nil,
			"param":   nil,
		},
	})
	if err != nil {
		return body
	}
	return out
}
