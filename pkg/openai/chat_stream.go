package openai

import (
	"context"
	"encoding/json"
	"net/http"
)

type ChatCompletionStreamChoiceDelta struct {
	Content   string     `json:"content,omitempty"`
	Role      string     `json:"role,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Refusal   string     `json:"refusal,omitempty"`

	// This property is used for reasoning-capable models.
	// which is not in the official documentation.
	// the doc from deepseek:
	// - https://api-docs.deepseek.com/api/create-chat-completion#responses
	//
	// As in ChatCompletionMessage, a delta naming it reasoning is decoded here
	// too, so a caller reads one field whichever server produced the stream.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

func (d *ChatCompletionStreamChoiceDelta) UnmarshalJSON(data []byte) error {
	// Decoded in two passes rather than through an embedded alias struct: a
	// decode error names the path it failed on, and an embedded field would
	// insert its own Go name into that path, in an error a user reads.
	type plain ChatCompletionStreamChoiceDelta
	if err := json.Unmarshal(data, (*plain)(d)); err != nil {
		return err
	}
	if d.ReasoningContent != "" {
		return nil
	}
	var alias struct {
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	d.ReasoningContent = alias.Reasoning
	return nil
}

type ChatCompletionStreamChoiceLogprobs struct {
	Content []ChatCompletionTokenLogprob `json:"content,omitempty"`
	Refusal []ChatCompletionTokenLogprob `json:"refusal,omitempty"`
}

type ChatCompletionTokenLogprob struct {
	Token       string                                 `json:"token"`
	Bytes       []int64                                `json:"bytes,omitempty"`
	Logprob     float64                                `json:"logprob,omitempty"`
	TopLogprobs []ChatCompletionTokenLogprobTopLogprob `json:"top_logprobs"`
}

type ChatCompletionTokenLogprobTopLogprob struct {
	Token   string  `json:"token"`
	Bytes   []int64 `json:"bytes"`
	Logprob float64 `json:"logprob"`
}

type ChatCompletionStreamChoice struct {
	Index        int                                 `json:"index"`
	Delta        ChatCompletionStreamChoiceDelta     `json:"delta"`
	Logprobs     *ChatCompletionStreamChoiceLogprobs `json:"logprobs,omitempty"`
	FinishReason FinishReason                        `json:"finish_reason"`
}

type ChatCompletionStreamResponse struct {
	ID                string                       `json:"id"`
	Object            string                       `json:"object"`
	Created           int64                        `json:"created"`
	Model             string                       `json:"model"`
	Choices           []ChatCompletionStreamChoice `json:"choices"`
	SystemFingerprint string                       `json:"system_fingerprint"`
	// An optional field that will only be present when you set stream_options: {"include_usage": true} in your request.
	// When present, it contains a null value except for the last chunk which contains the token usage statistics
	// for the entire request.
	Usage *Usage `json:"usage,omitempty"`
}

type ChatCompletionStream struct {
	*streamReader
}

// CreateChatCompletionStream - API call to create a chat completion w/ streaming
// support. It sets whether to stream back partial progress. If set, tokens will be
// sent as data-only server-sent events as they become available, with the
// stream terminated by a data: [DONE] message.
func (c *Client) CreateChatCompletionStream(ctx context.Context, request ChatCompletionRequest) (*ChatCompletionStream, error) {
	request.Stream = true

	req, err := c.newRequest(
		ctx,
		http.MethodPost,
		c.fullURL(chatCompletionsSuffix),
		request,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.sendRequestStream(req)
	if err != nil {
		return nil, err
	}

	stream := &ChatCompletionStream{
		streamReader: resp,
	}
	return stream, nil
}
