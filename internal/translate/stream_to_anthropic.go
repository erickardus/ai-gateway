package translate

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// openAIToAnthropicStream converts a Chat Completions SSE stream into a Messages
// API one.
//
// The two streams are shaped differently in a way that makes this a state
// machine rather than a mapping. OpenAI emits a flat sequence of deltas and
// says nothing about where one piece of content ends and the next begins;
// Anthropic emits an explicitly bracketed structure — every run of text, every
// tool call and every thinking passage is opened and closed by its own event,
// and a client that receives a delta for a block that was never opened treats
// the stream as corrupt. So the boundaries OpenAI leaves implicit are inferred
// here, from the point at which the kind of content changes.
//
// Text and reasoning are relayed as they arrive. Tool calls are not: their
// arguments are accumulated per upstream index and emitted as complete blocks
// once the stream ends. That costs the progressive rendering of a tool call and
// buys the only property that matters more — that the arguments are never
// spliced. OpenAI numbers its tool calls and is free to interleave their
// fragments, while an Anthropic block, once closed, cannot be reopened; a
// translator that streamed them live would have to either reopen a closed block
// or route a fragment into the wrong one, and both produce a tool call whose
// arguments are invalid JSON assembled from two different calls. Text, which is
// the bulk of a reply and the part a reader watches, still streams.
//
// Nothing is emitted until the upstream's first chunk arrives, because
// message_start carries the id and model that only that chunk knows. Nothing is
// closed until the stream ends, because several OpenAI-compatible servers send
// their usage in a final chunk that follows the finish reason, and closing on
// the finish reason would mean billing every streamed reply as nothing.
type openAIToAnthropicStream struct {
	started  bool
	finished bool

	id    string
	model string

	// openKind is the sort of content block currently open ("text" or
	// "thinking"), and openIndex its Anthropic content index. Empty means no
	// block is open.
	openKind  string
	openIndex int
	nextIndex int

	// tools accumulates each tool call by the upstream's own index for it,
	// which is what identifies a call across interleaved fragments. seen keeps
	// their first-seen order so the blocks are emitted as the model produced
	// them rather than in whatever order a map iterates.
	tools map[int]*pendingTool
	seen  []int

	stopReason string
	usage      *openAIUsage
}

// pendingTool is one tool call being assembled from its fragments.
type pendingTool struct {
	id   string
	name string
	args strings.Builder
}

func newOpenAIToAnthropicStream() *openAIToAnthropicStream {
	return &openAIToAnthropicStream{tools: map[int]*pendingTool{}}
}

func (s *openAIToAnthropicStream) line(line []byte, out *bytes.Buffer) {
	if isDone(line) {
		s.finish(out)
		return
	}
	payload := eventPayload(line)
	if payload == nil {
		return
	}

	// An upstream that fails mid-stream sends an error object where a chunk
	// belongs. Anthropic's stream has an event for exactly this, and a client
	// that receives it stops waiting; one that receives an unrecognized object
	// waits out its own timeout instead.
	if errEvent, ok := streamErrorEvent(payload); ok {
		if !s.started {
			s.start(openAIResponse{}, out)
		}
		writeEvent(out, "error", errEvent)
		s.finish(out)
		return
	}

	var chunk openAIResponse
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	if !s.started {
		s.start(chunk, out)
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return
	}

	choice := chunk.Choices[0]
	if delta := choice.Delta; delta != nil {
		if delta.ReasoningContent != "" {
			s.openBlock(out, "thinking", map[string]any{"type": "thinking", "thinking": ""})
			writeEvent(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.openIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": delta.ReasoningContent},
			})
		}
		if text := openAIContentText(delta.Content); text != "" {
			s.openBlock(out, "text", map[string]any{"type": "text", "text": ""})
			writeEvent(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.openIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
		}
		for _, call := range delta.ToolCalls {
			s.collectTool(call)
		}
	}
	if choice.FinishReason != "" {
		s.stopReason = stopReasonToAnthropic(choice.FinishReason)
	}
}

// eof closes a message the upstream left open. A stream that died half way
// still produced a reply, and a client holding an unterminated message waits
// for an event that is never coming.
func (s *openAIToAnthropicStream) eof(out *bytes.Buffer) { s.finish(out) }

func (s *openAIToAnthropicStream) start(chunk openAIResponse, out *bytes.Buffer) {
	s.started = true
	s.id, s.model = messageID(chunk.ID), chunk.Model
	// The input counts are not knowable yet: an OpenAI-compatible server
	// reports usage at the end of the stream, not the beginning. They are
	// carried on message_delta instead, which is where Anthropic itself
	// revises the figures, and where the gateway's own usage sniffer reads
	// them.
	writeEvent(out, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.id,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// openBlock ensures a block of the given kind is open, closing whatever else
// was.
func (s *openAIToAnthropicStream) openBlock(out *bytes.Buffer, kind string, block map[string]any) {
	if s.openKind == kind {
		return
	}
	s.closeBlock(out)
	s.openKind, s.openIndex = kind, s.nextIndex
	s.nextIndex++
	writeEvent(out, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.openIndex,
		"content_block": block,
	})
}

func (s *openAIToAnthropicStream) closeBlock(out *bytes.Buffer) {
	if s.openKind == "" {
		return
	}
	writeEvent(out, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": s.openIndex,
	})
	s.openKind = ""
}

// collectTool folds one upstream fragment into the call it belongs to.
//
// The upstream's own index identifies the call, not the order the fragments
// arrive in: a server emitting two calls may interleave their argument
// fragments, and keying on arrival order would splice one call's arguments into
// the other's. The id and name arrive on the fragment that opens a call and on
// no other, so each is kept the first time it is seen.
func (s *openAIToAnthropicStream) collectTool(call openAIToolCall) {
	index := 0
	if call.Index != nil {
		index = *call.Index
	}
	pending, ok := s.tools[index]
	if !ok {
		pending = &pendingTool{}
		s.tools[index] = pending
		s.seen = append(s.seen, index)
	}
	if pending.id == "" {
		pending.id = call.ID
	}
	if pending.name == "" {
		pending.name = call.Function.Name
	}
	pending.args.WriteString(call.Function.Arguments)
}

// emitTools writes the accumulated tool calls as complete Anthropic blocks, in
// the order the upstream first mentioned them.
func (s *openAIToAnthropicStream) emitTools(out *bytes.Buffer) {
	if len(s.seen) == 0 {
		return
	}
	// First-seen order is the model's order. Sorting the surviving indexes
	// keeps the numbering stable where a server mentions them out of order.
	order := append([]int(nil), s.seen...)
	sort.SliceStable(order, func(i, j int) bool { return order[i] < order[j] })

	for _, index := range order {
		pending := s.tools[index]
		writeEvent(out, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": s.nextIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    toolUseID(pending.id),
				"name":  pending.name,
				"input": map[string]any{},
			},
		})
		// The arguments are relayed as the upstream wrote them, in one delta.
		// They are the caller's document to interpret; re-encoding them here
		// would reorder their keys, and a partially received fragment is not
		// valid JSON to re-encode in the first place.
		if args := pending.args.String(); args != "" {
			writeEvent(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.nextIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
		}
		writeEvent(out, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": s.nextIndex,
		})
		s.nextIndex++
	}
}

// finish closes the message exactly once, whether the stream ended with the
// upstream's [DONE] sentinel or simply stopped.
func (s *openAIToAnthropicStream) finish(out *bytes.Buffer) {
	if s.finished {
		return
	}
	if !s.started {
		// The upstream produced no chunk at all. There is nothing to close and
		// nothing to say about it that would not be an invented reply.
		s.finished = true
		return
	}
	s.finished = true
	s.closeBlock(out)
	s.emitTools(out)

	stop := s.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	usage := usageToAnthropic(s.usage)
	writeEvent(out, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": usage,
	})
	writeEvent(out, "message_stop", map[string]any{"type": "message_stop"})
}

// streamErrorEvent recognizes an error object sent where a chunk belongs, and
// renders it as the Anthropic error event.
func streamErrorEvent(payload []byte) (map[string]any, bool) {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.Error == nil {
		return nil, false
	}
	kind := probe.Error.Type
	if kind == "" {
		kind = "api_error"
	}
	return map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": probe.Error.Message},
	}, true
}
