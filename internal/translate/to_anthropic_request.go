package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// defaultMaxTokens bounds a translated request whose OpenAI original named no
// output cap. The Messages API requires max_tokens and Chat Completions does
// not, so one has to be invented; this is large enough not to truncate ordinary
// replies and small enough not to be a surprise on the bill. Operators override
// it with router.translation.default_max_tokens.
const defaultMaxTokens = 8192

// minThinkingBudget is Anthropic's floor for extended thinking. A budget below
// it is refused outright, so an effort band that cannot clear it is dropped
// rather than sent.
const minThinkingBudget = 1024

// openAIRequestToAnthropic rewrites a Chat Completions request as a Messages
// API request.
//
// Four structural differences drive the work here:
//
//   - The system prompt is a message in one format and a top-level field in the
//     other, so every system message is hoisted out of the conversation.
//   - A tool result is a message of its own in one format and a block inside a
//     user turn in the other, so runs of tool messages collapse into one turn.
//   - The Messages API requires roles to alternate, which Chat Completions does
//     not, so adjacent turns of the same role are merged.
//   - max_tokens is required in one format and optional in the other, so a
//     request that omitted it is given a cap rather than refused.
func openAIRequestToAnthropic(body []byte, opts Options) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode openai request: %w", err)
	}

	out := anthropicRequest{
		Model:         in.Model,
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		StopSequences: in.Stop,
		Stream:        in.Stream,
	}

	maxTokens := opts.DefaultMaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	if in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0 {
		maxTokens = *in.MaxCompletionTokens
	} else if in.MaxTokens != nil && *in.MaxTokens > 0 {
		maxTokens = *in.MaxTokens
	}
	out.MaxTokens = maxTokens

	var (
		system   []string
		messages []anthropicMessage
	)
	for i, m := range in.Messages {
		// "developer" is the newer spelling of a system message and means the
		// same thing here.
		if m.Role == "system" || m.Role == "developer" {
			if text := openAIContentText(m.Content); text != "" {
				system = append(system, text)
			}
			continue
		}
		msg, err := openAIMessageToAnthropic(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if msg != nil {
			messages = append(messages, *msg)
		}
	}
	if len(system) > 0 {
		encoded, err := json.Marshal(strings.Join(system, "\n\n"))
		if err != nil {
			return nil, err
		}
		out.System = encoded
	}
	merged, err := mergeAdjacentRoles(messages)
	if err != nil {
		return nil, err
	}
	out.Messages = merged
	if out.Messages == nil {
		// The Messages API requires the member to be present, and an empty
		// array is a clearer refusal from the upstream than a null.
		out.Messages = []anthropicMessage{}
	}

	// tool_choice "none" has no Messages API spelling. Withholding the tools is
	// the only faithful way to say it: the model then cannot call one, which is
	// what the caller asked for.
	suppressTools := len(in.ToolChoice) > 0 && string(in.ToolChoice) == `"none"`
	if !suppressTools {
		for _, t := range in.Tools {
			if t.Type != "" && t.Type != "function" {
				continue
			}
			if t.Function.Name == "" {
				continue
			}
			schema := t.Function.Parameters
			if len(schema) == 0 {
				// input_schema is required. An object with no properties is
				// the faithful reading of a function that takes no arguments.
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out.Tools = append(out.Tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: schema,
			})
		}
		out.ToolChoice = toolChoiceToAnthropic(in.ToolChoice, in.ParallelToolCalls, len(out.Tools) > 0)
	}

	applyReasoningEffort(&out, in.ReasoningEffort)

	return json.Marshal(out)
}

// applyReasoningEffort turns OpenAI's effort band into an extended-thinking
// budget, and stands down wherever the result would be refused.
//
// Two constraints make this conditional rather than a lookup. Anthropic rejects
// a budget below its floor, and rejects one that does not leave room inside
// max_tokens for an answer — so a small cap means no thinking rather than a
// failed request. And it rejects temperature and top_p outright while thinking
// is on, so enabling it means dropping the sampling knobs the caller set. The
// caller asked for reasoning, so reasoning is what wins; Loss records the trade.
func applyReasoningEffort(out *anthropicRequest, effort string) {
	var budget int
	switch effort {
	case "minimal", "low":
		budget = minThinkingBudget
	case "medium":
		budget = 4096
	case "high":
		budget = 16384
	default:
		return
	}
	// Leave at least as much room for the reply as for the thinking, so a
	// request does not spend its whole cap reasoning and return nothing.
	if room := out.MaxTokens / 2; budget > room {
		budget = room
	}
	if budget < minThinkingBudget {
		return
	}
	out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	out.Temperature, out.TopP = nil, nil
}

// openAIMessageToAnthropic converts one non-system OpenAI message. It returns
// nil for a message that carries nothing at all, which is what an assistant
// turn whose only content was a refusal field decodes to.
func openAIMessageToAnthropic(m openAIMessage) (*anthropicMessage, error) {
	if m.Role == "tool" {
		block := anthropicBlock{
			Type:      "tool_result",
			ToolUseID: m.ToolCallID,
		}
		content, err := json.Marshal(openAIContentText(m.Content))
		if err != nil {
			return nil, err
		}
		block.Content = content
		return blockMessage("user", block)
	}

	var blocks []anthropicBlock
	if m.Role == "assistant" && m.ReasoningContent != "" {
		// Reasoning returned by an OpenAI-compatible server is unsigned, and
		// Anthropic refuses a thinking block it did not sign. It is carried as
		// text so the conversation keeps its shape rather than losing a turn.
		blocks = append(blocks, anthropicBlock{Type: "text", Text: m.ReasoningContent})
	}
	blocks = append(blocks, contentToBlocks(m.Content)...)
	for _, call := range m.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if len(input) == 0 || !json.Valid(input) {
			// An upstream that streamed nothing for a call, or a client
			// replaying one it truncated. An empty object keeps the turn
			// well-formed; the alternative is a 400 on the whole conversation.
			input = json.RawMessage(`{}`)
		}
		blocks = append(blocks, anthropicBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	if len(blocks) == 0 {
		return nil, nil
	}

	role := m.Role
	if role != "assistant" {
		role = "user"
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	return &anthropicMessage{Role: role, Content: encoded}, nil
}

func blockMessage(role string, blocks ...anthropicBlock) (*anthropicMessage, error) {
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	return &anthropicMessage{Role: role, Content: encoded}, nil
}

// contentToBlocks reads an OpenAI content value — a string, or an array of
// parts — as Anthropic content blocks.
func contentToBlocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: text}}
	}
	var parts []openAIPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	blocks := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			if src := sourceFromURL(p.ImageURL.URL); src != nil {
				blocks = append(blocks, anthropicBlock{Type: "image", Source: src})
			}
		}
	}
	return blocks
}

// sourceFromURL reads an OpenAI image URL as an Anthropic image source, taking
// a data URI apart into the media type and payload the Messages API wants.
func sourceFromURL(url string) *anthropicSource {
	if url == "" {
		return nil
	}
	if rest, found := strings.CutPrefix(url, "data:"); found {
		meta, data, ok := strings.Cut(rest, ",")
		if !ok {
			return nil
		}
		mediaType, encoding, _ := strings.Cut(meta, ";")
		if encoding != "base64" || mediaType == "" || data == "" {
			return nil
		}
		return &anthropicSource{Type: "base64", MediaType: mediaType, Data: data}
	}
	return &anthropicSource{Type: "url", URL: url}
}

// openAIContentText flattens an OpenAI content value to plain text.
func openAIContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []openAIPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" || p.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// mergeAdjacentRoles folds runs of same-role turns into one.
//
// Chat Completions permits them and the Messages API refuses them outright, so
// a conversation with two consecutive user messages — which is exactly what a
// run of tool results produces — would otherwise be rejected in full. Merging
// concatenates the blocks, which is the reading the caller meant.
func mergeAdjacentRoles(msgs []anthropicMessage) ([]anthropicMessage, error) {
	if len(msgs) < 2 {
		return msgs, nil
	}
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(out) == 0 || out[len(out)-1].Role != m.Role {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		var a, b []anthropicBlock
		if err := json.Unmarshal(prev.Content, &a); err != nil {
			return nil, fmt.Errorf("merge %s turns: %w", m.Role, err)
		}
		if err := json.Unmarshal(m.Content, &b); err != nil {
			return nil, fmt.Errorf("merge %s turns: %w", m.Role, err)
		}
		encoded, err := json.Marshal(append(a, b...))
		if err != nil {
			return nil, err
		}
		prev.Content = encoded
	}
	return out, nil
}

// toolChoiceToAnthropic maps OpenAI's tool_choice — two bare strings and an
// object — onto the Messages API's object form. "none" never reaches here: it
// is expressed by withholding the tools instead.
func toolChoiceToAnthropic(raw json.RawMessage, parallel *bool, hasTools bool) *anthropicToolPick {
	if !hasTools {
		return nil
	}
	pick := &anthropicToolPick{}
	if parallel != nil && !*parallel {
		disabled := true
		pick.DisableParallelToolUse = &disabled
	}

	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		switch name {
		case "auto":
			pick.Type = "auto"
		case "required", "any":
			pick.Type = "any"
		default:
			if pick.DisableParallelToolUse == nil {
				return nil
			}
			pick.Type = "auto"
		}
		return pick
	}

	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err != nil || named.Function.Name == "" {
		if pick.DisableParallelToolUse == nil {
			return nil
		}
		pick.Type = "auto"
		return pick
	}
	pick.Type, pick.Name = "tool", named.Function.Name
	return pick
}
