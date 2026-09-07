package translate

import (
	"bytes"
	"encoding/json"
	"time"
)

// anthropicToOpenAIStream converts a Messages API SSE stream into a Chat
// Completions one.
//
// The direction is easier than its opposite only because information is being
// discarded rather than invented: Anthropic states every boundary explicitly,
// and OpenAI's stream has nowhere to put most of them. What has to be tracked
// is the tool-call numbering. Anthropic indexes every content block in one
// sequence — text and tool calls share it — while OpenAI numbers tool calls in
// a sequence of their own, and a client assembling arguments keys on that
// number. Handing it Anthropic's index would leave gaps a client reads as
// missing calls.
type anthropicToOpenAIStream struct {
	started  bool
	finished bool

	id      string
	model   string
	created int64

	// blockKind maps an Anthropic content index to what it holds, so a delta
	// can be routed without re-reading the block that opened it.
	blockKind map[int]string
	// toolIndex maps an Anthropic content index to OpenAI's own tool-call
	// numbering.
	toolIndex     map[int]int
	nextToolIndex int

	stopReason string
	usage      anthropicUsage
}

func newAnthropicToOpenAIStream() *anthropicToOpenAIStream {
	return &anthropicToOpenAIStream{
		blockKind: map[int]string{},
		toolIndex: map[int]int{},
		created:   time.Now().Unix(),
	}
}

// anthropicEvent is every streamed event this direction reads. Fields belonging
// to other event types decode as zero, so one struct covers the sequence.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string         `json:"id"`
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *anthropicToOpenAIStream) line(line []byte, out *bytes.Buffer) {
	payload := eventPayload(line)
	if payload == nil {
		return
	}
	var ev anthropicEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}

	switch ev.Type {
	case "message_start":
		s.started = true
		if ev.Message != nil {
			s.id, s.model = completionID(ev.Message.ID), ev.Message.Model
			s.mergeUsage(ev.Message.Usage)
		}
		// OpenAI opens with a chunk that carries only the role, which is what
		// a client waits for before it starts assembling anything.
		writeData(out, s.chunk(map[string]any{"role": "assistant"}, ""))

	case "content_block_start":
		if ev.ContentBlock == nil {
			return
		}
		s.blockKind[ev.Index] = ev.ContentBlock.Type
		if ev.ContentBlock.Type != "tool_use" {
			return
		}
		index := s.nextToolIndex
		s.nextToolIndex++
		s.toolIndex[ev.Index] = index
		writeData(out, s.chunk(map[string]any{
			"tool_calls": []map[string]any{{
				"index":    index,
				"id":       ev.ContentBlock.ID,
				"type":     "function",
				"function": map[string]string{"name": ev.ContentBlock.Name, "arguments": ""},
			}},
		}, ""))

	case "content_block_delta":
		if ev.Delta == nil {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				writeData(out, s.chunk(map[string]any{"content": ev.Delta.Text}, ""))
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				writeData(out, s.chunk(map[string]any{"reasoning_content": ev.Delta.Thinking}, ""))
			}
		case "input_json_delta":
			index, ok := s.toolIndex[ev.Index]
			if !ok || ev.Delta.PartialJSON == "" {
				return
			}
			writeData(out, s.chunk(map[string]any{
				"tool_calls": []map[string]any{{
					"index":    index,
					"function": map[string]string{"arguments": ev.Delta.PartialJSON},
				}},
			}, ""))
		case "signature_delta":
			// A thinking block's signature proves to Anthropic that the
			// reasoning is its own. There is nothing in the OpenAI stream that
			// carries it and no client that would send it back, so it is
			// dropped rather than rendered as content the user would read.
		}

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.stopReason = stopReasonToOpenAI(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			s.mergeUsage(*ev.Usage)
		}

	case "message_stop":
		s.finish(out)

	case "error":
		if ev.Error == nil {
			return
		}
		kind := ev.Error.Type
		if kind == "" {
			kind = "api_error"
		}
		writeData(out, map[string]any{
			"error": map[string]any{"type": kind, "message": ev.Error.Message, "code": nil, "param": nil},
		})
		s.finish(out)

	case "ping":
		// Relayed as an SSE comment. It carries no content, but it is the only
		// traffic during a long thinking pause, and a client counting silence
		// against a timeout needs the bytes to keep arriving.
		out.WriteString(": ping\n\n")
	}
}

// eof terminates a stream the upstream left open, so a client is not left
// waiting on a [DONE] that will never arrive.
func (s *anthropicToOpenAIStream) eof(out *bytes.Buffer) { s.finish(out) }

// mergeUsage keeps the largest figure seen for each counter. A streamed reply
// reports them across several events and never revises one downwards, so the
// maximum is the total.
func (s *anthropicToOpenAIStream) mergeUsage(next anthropicUsage) {
	s.usage.InputTokens = max(s.usage.InputTokens, next.InputTokens)
	s.usage.OutputTokens = max(s.usage.OutputTokens, next.OutputTokens)
	s.usage.CacheReadInputTokens = max(s.usage.CacheReadInputTokens, next.CacheReadInputTokens)
	s.usage.CacheCreationInputTokens = max(s.usage.CacheCreationInputTokens, next.CacheCreationInputTokens)
	if len(next.CacheCreation) > 0 {
		s.usage.CacheCreation = next.CacheCreation
	}
}

func (s *anthropicToOpenAIStream) finish(out *bytes.Buffer) {
	if s.finished || !s.started {
		s.finished = true
		return
	}
	s.finished = true

	stop := s.stopReason
	if stop == "" {
		stop = "stop"
	}
	writeData(out, s.chunk(map[string]any{}, stop))

	// Usage rides a final chunk with no choices, which is where OpenAI itself
	// puts it when a caller asked for it.
	usage := s.chunkEnvelope()
	usage["choices"] = []any{}
	usage["usage"] = usageToOpenAI(s.usage)
	writeData(out, usage)

	out.WriteString("data: [DONE]\n\n")
}

// chunk builds one streamed completion chunk around a delta.
func (s *anthropicToOpenAIStream) chunk(delta map[string]any, finish string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	env := s.chunkEnvelope()
	env["choices"] = []any{choice}
	return env
}

func (s *anthropicToOpenAIStream) chunkEnvelope() map[string]any {
	id := s.id
	if id == "" {
		id = completionID("")
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
	}
}
