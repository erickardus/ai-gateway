package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicRequestToOpenAI rewrites a Messages API request as a Chat
// Completions request.
//
// The shapes differ in three ways that are not cosmetic, and each is handled
// here rather than left to the upstream to reject:
//
//   - Anthropic carries the system prompt in a top-level field; OpenAI carries
//     it as the first message. Anthropic also allows it to be an array of
//     blocks, which flattens to one string.
//   - Anthropic puts a tool result inside a user turn; OpenAI gives it a role
//     of its own. A user turn holding results therefore becomes several
//     messages, and the tool messages must come first, because OpenAI requires
//     each to answer the assistant turn that called it.
//   - Anthropic puts a tool call inside an assistant turn's content; OpenAI
//     hangs it off the message as tool_calls, with the arguments as a string.
func anthropicRequestToOpenAI(body []byte, opts Options) ([]byte, error) {
	var in anthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode anthropic request: %w", err)
	}

	out := openAIRequest{
		Model:       in.Model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.StopSequences,
		Stream:      in.Stream,
	}
	if in.MaxTokens > 0 {
		n := in.MaxTokens
		if opts.MaxCompletionTokens {
			out.MaxCompletionTokens = &n
		} else {
			out.MaxTokens = &n
		}
	}

	if sys, err := systemToMessage(in.System, opts); err != nil {
		return nil, err
	} else if sys != nil {
		out.Messages = append(out.Messages, *sys)
	}

	for i, m := range in.Messages {
		msgs, err := anthropicMessageToOpenAI(m, opts)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for _, t := range in.Tools {
		// A server-side tool is named by Type and has no function schema to
		// send. Forwarding it as a function would offer the upstream a tool it
		// cannot run and invite a call the gateway could not answer.
		if t.Name == "" || (t.Type != "" && t.Type != "custom" && t.InputSchema == nil) {
			continue
		}
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	if choice, parallel := toolChoiceToOpenAI(in.ToolChoice, len(out.Tools) > 0); choice != nil {
		out.ToolChoice = choice
		out.ParallelToolCalls = parallel
	}

	// Extended thinking becomes an effort band. The budget is a token count and
	// the effort is a three-valued knob, so the mapping is lossy in one
	// direction and unrecoverable in the other; Loss says as much.
	if in.Thinking != nil && in.Thinking.Type == "enabled" {
		out.ReasoningEffort = effortForBudget(in.Thinking.BudgetTokens)
	}

	return json.Marshal(out)
}

// effortForBudget bands a thinking budget into the effort levels OpenAI takes.
// The boundaries follow Anthropic's own minimum budget of 1024 tokens: anything
// at or near the floor is the shallowest setting available, and the top band
// starts where a budget stops being a hint and starts being a plan.
func effortForBudget(budget int) string {
	switch {
	case budget <= 0:
		return "medium"
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// systemToMessage turns Anthropic's top-level system field — a string, or an
// array of text blocks — into OpenAI's leading system message.
func systemToMessage(raw json.RawMessage, opts Options) (*openAIMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		content, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		return &openAIMessage{Role: "system", Content: content}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("decode system: %w", err)
	}
	parts := make([]openAIPart, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		part := openAIPart{Type: "text", Text: b.Text}
		if opts.KeepCacheControl {
			part.CacheControl = b.CacheControl
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	content, err := encodeParts(parts, opts)
	if err != nil {
		return nil, err
	}
	return &openAIMessage{Role: "system", Content: content}, nil
}

// anthropicMessageToOpenAI expands one Anthropic turn into the one or more
// OpenAI messages that say the same thing.
func anthropicMessageToOpenAI(m anthropicMessage, opts Options) ([]openAIMessage, error) {
	// A string content is the common case and needs no expansion at all.
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		content, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		return []openAIMessage{{Role: m.Role, Content: content}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode content: %w", err)
	}

	var (
		toolMsgs []openAIMessage
		parts    []openAIPart
		calls    []openAIToolCall
		prose    strings.Builder
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			part := openAIPart{Type: "text", Text: b.Text}
			if opts.KeepCacheControl {
				part.CacheControl = b.CacheControl
			}
			parts = append(parts, part)
			if prose.Len() > 0 {
				prose.WriteString("\n")
			}
			prose.WriteString(b.Text)
		case "image":
			if url := imageURL(b.Source); url != "" {
				parts = append(parts, openAIPart{Type: "image_url", ImageURL: &openAIImageURL{URL: url}})
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			calls = append(calls, openAIToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: openAIToolCallFunction{Name: b.Name, Arguments: args},
			})
		case "tool_result":
			content, err := json.Marshal(toolResultText(b))
			if err != nil {
				return nil, err
			}
			toolMsgs = append(toolMsgs, openAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    content,
			})
		case "thinking", "redacted_thinking", "document":
			// Dropped. A thinking block carries a signature that only
			// Anthropic can verify and only Anthropic will accept back, so
			// there is nothing to send an OpenAI server that it could use;
			// replaying the text as prose would put the model's private
			// reasoning into the conversation as if the user had said it.
		}
	}

	// Tool results answer the assistant turn that made the calls, so they must
	// precede whatever else the user said in the same Anthropic turn.
	out := toolMsgs
	switch {
	case m.Role == "assistant":
		msg := openAIMessage{Role: "assistant", ToolCalls: calls}
		if prose.Len() > 0 {
			// An assistant turn's content is a plain string. The parts form is
			// accepted for user input and refused by several servers here.
			content, err := json.Marshal(prose.String())
			if err != nil {
				return nil, err
			}
			msg.Content = content
		}
		if msg.Content != nil || len(msg.ToolCalls) > 0 {
			out = append(out, msg)
		}
	case len(parts) > 0:
		content, err := encodeParts(parts, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, openAIMessage{Role: m.Role, Content: content})
	}
	return out, nil
}

// encodeParts writes a content value as the simplest shape that carries it: a
// bare string where the turn is only text and nothing is marked, and the parts
// array otherwise. The plain string is not merely tidier — several
// OpenAI-compatible servers accept only that form on some roles.
func encodeParts(parts []openAIPart, opts Options) (json.RawMessage, error) {
	simple := true
	for _, p := range parts {
		if p.Type != "text" || len(p.CacheControl) > 0 {
			simple = false
			break
		}
	}
	if simple {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			texts = append(texts, p.Text)
		}
		return json.Marshal(strings.Join(texts, "\n"))
	}
	return json.Marshal(parts)
}

// imageURL renders an Anthropic image source as the URL an OpenAI image part
// takes, inlining base64 data as a data URI.
func imageURL(src *anthropicSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "url":
		return src.URL
	case "base64":
		if src.Data == "" || src.MediaType == "" {
			return ""
		}
		return "data:" + src.MediaType + ";base64," + src.Data
	}
	return ""
}

// toolResultText flattens a tool result to the string an OpenAI tool message
// carries. A result given as blocks keeps its text; an image inside one is lost,
// because a tool message takes no image parts at all.
func toolResultText(b anthropicBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(b.Content, &text); err == nil {
		return text
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		// Not a shape this package knows: hand the upstream the document as
		// written rather than an empty result, which would read as a tool that
		// returned nothing.
		return string(b.Content)
	}
	var sb strings.Builder
	for _, inner := range blocks {
		if inner.Type != "text" || inner.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(inner.Text)
	}
	return sb.String()
}

// toolChoiceToOpenAI maps Anthropic's tool_choice object onto OpenAI's, which
// spells two of the three values as bare strings.
//
// It returns nothing when the request declared no usable tools: "required"
// against an empty tool list is a 400 on every server that checks.
func toolChoiceToOpenAI(pick *anthropicToolPick, hasTools bool) (json.RawMessage, *bool) {
	if pick == nil || !hasTools {
		return nil, nil
	}
	var parallel *bool
	if pick.DisableParallelToolUse != nil {
		allowed := !*pick.DisableParallelToolUse
		parallel = &allowed
	}
	switch pick.Type {
	case "auto":
		return json.RawMessage(`"auto"`), parallel
	case "any":
		return json.RawMessage(`"required"`), parallel
	case "none":
		return json.RawMessage(`"none"`), parallel
	case "tool":
		if pick.Name == "" {
			return json.RawMessage(`"required"`), parallel
		}
		named, err := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": pick.Name},
		})
		if err != nil {
			return nil, parallel
		}
		return named, parallel
	}
	return nil, parallel
}
