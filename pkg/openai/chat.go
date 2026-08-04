package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

const chatCompletionsSuffix = "/chat/completions"

type ImageURLDetail string

const (
	ImageURLDetailHigh ImageURLDetail = "high"
	ImageURLDetailLow  ImageURLDetail = "low"
	ImageURLDetailAuto ImageURLDetail = "auto"
)

type ChatMessageImageURL struct {
	URL    string         `json:"url,omitempty"`
	Detail ImageURLDetail `json:"detail,omitempty"`
}

type ChatMessagePartType string

const (
	ChatMessagePartTypeText     ChatMessagePartType = "text"
	ChatMessagePartTypeImageURL ChatMessagePartType = "image_url"
)

type ChatMessagePart struct {
	Type     ChatMessagePartType  `json:"type,omitempty"`
	Text     string               `json:"text,omitempty"`
	ImageURL *ChatMessageImageURL `json:"image_url,omitempty"`
}

type ChatCompletionMessage struct {
	Role         Role   `json:"role"`
	Content      string `json:"content,omitempty"`
	Refusal      string `json:"refusal,omitempty"`
	MultiContent []ChatMessagePart

	// This property isn't in the official documentation, but it's in
	// the documentation for the official library for python:
	// - https://github.com/openai/openai-python/blob/main/chatml.md
	// - https://github.com/openai/openai-cookbook/blob/main/examples/How_to_count_tokens_with_tiktoken.ipynb
	Name string `json:"name,omitempty"`

	// This property is used for reasoning-capable models.
	// which is not in the official documentation.
	// the doc from deepseek:
	// - https://api-docs.deepseek.com/api/create-chat-completion#responses
	//
	// Servers do not agree on the name: DeepSeek returns reasoning_content,
	// while vLLM and OpenRouter return reasoning. Both are decoded into this
	// field; requests keep sending reasoning_content, which every one of them
	// accepts.
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// ToolCalls contains the tools the model wants to call (only valid for Role=assistant).
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// For Role=tool prompts this should be set to the ID given in the assistant's prior request to call a tool.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

func (m ChatCompletionMessage) MarshalJSON() ([]byte, error) {
	if m.Content != "" && m.MultiContent != nil {
		return nil, errors.New("can't use both Content and MultiContent simultaneously")
	}
	type wire struct {
		Role             Role       `json:"role"`
		Content          any        `json:"content"`
		Refusal          string     `json:"refusal,omitempty"`
		Name             string     `json:"name,omitempty"`
		ReasoningContent string     `json:"reasoning_content,omitempty"`
		ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string     `json:"tool_call_id,omitempty"`
	}

	w := wire{
		Role:             m.Role,
		Content:          m.Content, // string, "" included — always on the wire
		Refusal:          m.Refusal,
		Name:             m.Name,
		ReasoningContent: m.ReasoningContent,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
	}
	if len(m.MultiContent) > 0 {
		w.Content = m.MultiContent
	}
	return json.Marshal(w)
}

func (m *ChatCompletionMessage) UnmarshalJSON(data []byte) error {
	type Alias ChatCompletionMessage
	aux := struct {
		*Alias
		Content   json.RawMessage `json:"content"`
		Reasoning string          `json:"reasoning"`
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// reasoning_content takes precedence, so a server sending both is read the
	// way it was before this alias existed.
	if m.ReasoningContent == "" {
		m.ReasoningContent = aux.Reasoning
	}

	contentBytes := bytes.TrimSpace(aux.Content)
	if bytes.Equal(contentBytes, []byte("null")) || len(contentBytes) == 0 {
		return nil
	}

	// Check the first byte to determine if it's a string or an array
	switch contentBytes[0] {
	case '"':
		return json.Unmarshal(contentBytes, &m.Content)
	case '[':
		return json.Unmarshal(contentBytes, &m.MultiContent)
	}

	return fmt.Errorf("unexpected content type in chat message: %s", string(contentBytes))
}

type ToolCall struct {
	// Index is only populated in streaming chunk responses.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     ToolType     `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments is a JSON-encoded string containing the function arguments.
	Arguments string `json:"arguments,omitempty"`
}

type ChatCompletionResponseFormatType string

const (
	ChatCompletionResponseFormatTypeJSONObject ChatCompletionResponseFormatType = "json_object"
	ChatCompletionResponseFormatTypeJSONSchema ChatCompletionResponseFormatType = "json_schema"
	ChatCompletionResponseFormatTypeText       ChatCompletionResponseFormatType = "text"
)

type ChatCompletionResponseFormat struct {
	Type       ChatCompletionResponseFormatType        `json:"type,omitempty"`
	JSONSchema *ChatCompletionResponseFormatJSONSchema `json:"json_schema,omitempty"`
}

type ChatCompletionResponseFormatJSONSchema struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema"`
	Strict      bool   `json:"strict"`
}

// ChatCompletionRequestExtensions contains third-party OpenAI API extensions
// (e.g., vendor-specific implementations like vLLM).
type ChatCompletionRequestExtensions struct {
	// GuidedChoice is a vLLM-specific extension that restricts the model's output
	// to one of the predefined string choices provided in this field. This feature
	// is used to constrain the model's responses to a controlled set of options,
	// ensuring predictable and consistent outputs in scenarios where specific
	// choices are required.
	GuidedChoice []string `json:"guided_choice,omitempty"`
}

type ChatCompletionRequest struct {
	Model    string                  `json:"model"`
	Messages []ChatCompletionMessage `json:"messages"`

	MaxTokens int `json:"max_tokens,omitempty"`
	// MaxCompletionTokens An upper bound for the number of tokens that can be generated for a completion,
	// including visible output tokens and reasoning tokens https://platform.openai.com/docs/guides/reasoning
	MaxCompletionTokens int                           `json:"max_completion_tokens,omitempty"`
	Temperature         *float32                      `json:"temperature,omitempty"`
	TopP                *float32                      `json:"top_p,omitempty"`
	N                   int                           `json:"n,omitempty"`
	Stream              bool                          `json:"stream,omitempty"`
	Stop                []string                      `json:"stop,omitempty"`
	PresencePenalty     *float32                      `json:"presence_penalty,omitempty"`
	ResponseFormat      *ChatCompletionResponseFormat `json:"response_format,omitempty"`
	Seed                *int                          `json:"seed,omitempty"`
	FrequencyPenalty    *float32                      `json:"frequency_penalty,omitempty"`
	// LogitBias must be a token id string (specified by their token ID in the tokenizer), not a word string.
	// incorrect: `"logit_bias":{"You": 6}`, correct: `"logit_bias":{"1639": 6}`
	// refs: https://platform.openai.com/docs/api-reference/chat/create#chat/create-logit_bias
	LogitBias map[string]int `json:"logit_bias,omitempty"`
	// LogProbs indicates whether to return log probabilities of the output tokens or not.
	// If true, returns the log probabilities of each output token returned in the content of message.
	// This option is currently not available on the gpt-4-vision-preview model.
	LogProbs bool `json:"logprobs,omitempty"`
	// TopLogProbs is an integer between 0 and 5 specifying the number of most likely tokens to return at each
	// token position, each with an associated log probability.
	// logprobs must be set to true if this parameter is used.
	TopLogProbs    int    `json:"top_logprobs,omitempty"`
	User           string `json:"user,omitempty"`
	PromptCacheKey string `json:"prompt_cache_key,omitzero"`
	Tools          []Tool `json:"tools,omitempty"`
	// This can be either a string or an ToolChoice object.
	ToolChoice any `json:"tool_choice,omitempty"`
	// Options for streaming response. Only set this when you set stream: true.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	// Disable the default behavior of parallel tool calls by setting it: false.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// Store can be set to true to store the output of this completion request for use in distillations and evals.
	// https://platform.openai.com/docs/api-reference/chat/create#chat-create-store
	Store bool `json:"store,omitempty"`
	// Controls effort on reasoning models. Accepted values are provider-specific.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Reasoning is the normalized reasoning configuration used by gateways such
	// as OpenRouter. Direct OpenAI-compatible providers generally use the
	// top-level ReasoningEffort field instead.
	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`
	// Metadata to store with the completion.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Configuration for a predicted output.
	Prediction *Prediction `json:"prediction,omitempty"`
	// ChatTemplateKwargs provides a way to add non-standard parameters to the request body.
	// Additional kwargs to pass to the template renderer. Will be accessible by the chat template.
	// Such as think mode for qwen3. "chat_template_kwargs": {"enable_thinking": false}
	// https://qwen.readthedocs.io/en/latest/deployment/vllm.html#thinking-non-thinking-modes
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	// Specifies the latency tier to use for processing the request.
	ServiceTier ServiceTier `json:"service_tier,omitempty"`
	// Verbosity determines how many output tokens are generated. Lowering the number of
	// tokens reduces overall latency. It can be set to "low", "medium", or "high".
	// Note: This field is only confirmed to work with gpt-5, gpt-5-mini and gpt-5-nano.
	// Also, it is not in the API reference of chat completion at the time of writing,
	// though it is supported by the API.
	Verbosity string `json:"verbosity,omitempty"`
	// A stable identifier used to help detect users of your application that may be violating OpenAI's usage policies.
	// The IDs should be a string that uniquely identifies each user.
	// We recommend hashing their username or email address, in order to avoid sending us any identifying information.
	// https://platform.openai.com/docs/api-reference/chat/create#chat_create-safety_identifier
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// Embedded struct for non-OpenAI extensions
	ChatCompletionRequestExtensions
}

type ReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

type StreamOptions struct {
	// If set, an additional chunk will be streamed before the data: [DONE] message.
	// The usage field on this chunk shows the token usage statistics for the entire request,
	// and the choices field will always be an empty array.
	// All other chunks will also include a usage field, but with a null value.
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ToolType string

const (
	ToolTypeFunction ToolType = "function"
)

type Tool struct {
	Type     ToolType            `json:"type"`
	Function *FunctionDefinition `json:"function,omitempty"`
}

type ToolChoice struct {
	Type     ToolType      `json:"type"`
	Function *ToolFunction `json:"function,omitempty"`
}

type ToolFunction struct {
	Name string `json:"name"`
}

type FunctionDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Strict      bool   `json:"strict,omitempty"`
	// Parameters is an object describing the function.
	// You can pass json.RawMessage to describe the schema,
	// or you can pass in a struct which serializes to the proper JSON schema.
	Parameters any `json:"parameters"`
}

type TopLogProbs struct {
	Token   string  `json:"token"`
	LogProb float64 `json:"logprob"`
	Bytes   []byte  `json:"bytes,omitempty"`
}

// LogProb represents the probability information for a token.
type LogProb struct {
	Token   string  `json:"token"`
	LogProb float64 `json:"logprob"`
	Bytes   []byte  `json:"bytes,omitempty"` // Omitting the field if it is null
	// TopLogProbs is a list of the most likely tokens and their log probability, at this token position.
	// In rare cases, there may be fewer than the number of requested top_logprobs returned.
	TopLogProbs []TopLogProbs `json:"top_logprobs"`
}

// LogProbs is the top-level structure containing the log probability information.
type LogProbs struct {
	// Content is a list of message content tokens with log probability information.
	Content []LogProb `json:"content"`
}

type Prediction struct {
	Content string `json:"content"`
	Type    string `json:"type"`
}

type ChatCompletionChoice struct {
	Index   int                   `json:"index"`
	Message ChatCompletionMessage `json:"message"`
	// FinishReason indicates why the model stopped generating.
	// Common values: "stop" (natural completion), "length" (hit token limit),
	// "tool_calls" (model wants to call a tool), or "content_filter".
	FinishReason FinishReason `json:"finish_reason"`
	LogProbs     *LogProbs    `json:"logprobs,omitempty"`
}

type ChatCompletionResponse struct {
	ID                string                 `json:"id"`
	Object            string                 `json:"object"`
	Created           int64                  `json:"created"`
	Model             string                 `json:"model"`
	Choices           []ChatCompletionChoice `json:"choices"`
	Usage             Usage                  `json:"usage"`
	SystemFingerprint string                 `json:"system_fingerprint"`
	ServiceTier       ServiceTier            `json:"service_tier,omitempty"`
}

func (c *Client) CreateChatCompletion(ctx context.Context, request ChatCompletionRequest) (ChatCompletionResponse, error) {
	if request.Stream {
		return ChatCompletionResponse{}, errors.New("streaming is not supported with this method, please use CreateChatCompletionStream")
	}

	req, err := c.newRequest(
		ctx,
		http.MethodPost,
		c.fullURL(chatCompletionsSuffix),
		request,
	)
	if err != nil {
		return ChatCompletionResponse{}, err
	}

	var response ChatCompletionResponse
	err = c.sendRequest(req, &response)
	if err != nil {
		return ChatCompletionResponse{}, err
	}

	return response, nil
}

func UserMessage(content string) ChatCompletionMessage {
	return ChatCompletionMessage{
		Role:    RoleUser,
		Content: content,
	}
}

func SystemMessage(content string) ChatCompletionMessage {
	return ChatCompletionMessage{
		Role:    RoleSystem,
		Content: content,
	}
}

func AssistantMessage(content string) ChatCompletionMessage {
	return ChatCompletionMessage{
		Role:    RoleAssistant,
		Content: content,
	}
}
