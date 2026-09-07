package translate

import "encoding/json"

// The wire shapes both formats use. Only the members this package can actually
// carry across are declared: a field absent from these structs is a field
// translation drops, and Loss says so in words an operator can read.
//
// Schemas, tool arguments and tool results are held as json.RawMessage
// throughout. They are caller-authored documents that neither format
// interprets, and re-encoding one through a Go map would reorder its keys and
// renormalize its numbers — which is invisible in a request and fatal in a
// prompt-cache prefix, where the bytes are the key.

// ---------- Anthropic Messages ----------

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    *anthropicToolPick `json:"tool_choice,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock is every content block either direction needs to read. The
// blocks are distinguished by Type, and the fields of the others decode as
// zero, so one struct reads them all without a discriminated union.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image / document
	Source *anthropicSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Type marks Anthropic's server-side tools (computer_20250124 and the
	// like). They have no OpenAI counterpart and are dropped rather than
	// forwarded as a function the upstream would then try to call.
	Type string `json:"type,omitempty"`
}

type anthropicToolPick struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []anthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

// anthropicUsage reports input excluding both cache counters, which is the
// convention the whole gateway normalizes to. Writing it any other way here
// would bill a translated reply's cached tokens twice.
type anthropicUsage struct {
	InputTokens              int             `json:"input_tokens"`
	OutputTokens             int             `json:"output_tokens"`
	CacheReadInputTokens     int             `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            json.RawMessage `json:"cache_creation,omitempty"`
}

// ---------- OpenAI Chat Completions ----------

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	// MaxTokens and MaxCompletionTokens are the same cap under two names. The
	// newer reasoning models refuse the old one, and several
	// OpenAI-compatible servers have never heard of the new one, so which is
	// emitted is a per-deployment fact rather than a guess made here.
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       json.RawMessage `json:"stream_options,omitempty"`
	Tools               []openAITool    `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	// Name is carried through on a tool message by some servers and ignored by
	// others; it is never load-bearing, so it is not reconstructed.
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// ReasoningContent is how the OpenAI-compatible ecosystem returns a model's
	// visible reasoning. DeepSeek introduced it and several servers copied it;
	// OpenAI's own API does not return reasoning at all.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
	// CacheControl is Anthropic's marker, kept on the way across only for the
	// OpenAI-compatible servers whose explicit cache reads it — Alibaba's Qwen
	// is the one that does. Every other server would answer 400 to an unknown
	// member, so it is carried only where the operator declared the upstream
	// with supports_cache_control.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	Index    *int                   `json:"index,omitempty"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIToolCallFunction `json:"function"`
}

type openAIToolCallFunction struct {
	Name string `json:"name,omitempty"`
	// Arguments is a JSON document encoded as a string, and it arrives in
	// fragments on a streamed reply. It is concatenated as text and never
	// parsed: a half-received fragment is not valid JSON, and the completed
	// string is the caller's document to interpret rather than this package's.
	Arguments string `json:"arguments,omitempty"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Message      *openAIMessage `json:"message,omitempty"`
	Delta        *openAIMessage `json:"delta,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

// openAIUsage reports prompt_tokens *including* every cached token, with the
// breakdown beside it. The difference from Anthropic's convention is the whole
// reason usage is recomputed on the way across rather than copied.
type openAIUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *openAIPromptDetails `json:"prompt_tokens_details,omitempty"`
	// PromptCacheHitTokens is DeepSeek's top-level spelling of a cache read.
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
}

type openAIPromptDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
	// CacheReadInputTokens and CacheCreationInputTokens are what the
	// explicit-cache providers (Qwen, MiniMax) nest here under Anthropic's
	// names. Reading both placements is what keeps a write from being billed
	// at the input rate.
	CacheReadInputTokens     int             `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int             `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            json.RawMessage `json:"cache_creation,omitempty"`
}

// cacheRead returns the tokens served from a cache under whichever of the two
// names the provider used. They are one figure under two names, never a sum.
func (u *openAIUsage) cacheRead() int {
	if u == nil {
		return 0
	}
	read := u.PromptCacheHitTokens
	if d := u.PromptTokensDetails; d != nil {
		read = max(read, d.CachedTokens, d.CacheReadInputTokens)
	}
	return nonNegative(read)
}

// cacheWritten returns the tokens written into a cache, which only the
// explicit-cache providers report and only from inside the details object.
func (u *openAIUsage) cacheWritten() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return nonNegative(u.PromptTokensDetails.CacheCreationInputTokens)
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
