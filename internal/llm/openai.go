package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type openAIClient struct {
	client openai.Client
}

// NewOpenAI returns a Client for the OpenAI chat completions API or any server compatible with
// it, such as Ollama, LM Studio, or OpenRouter. An empty baseURL uses OpenAI, and a nil hc the SDK's HTTP client.
func NewOpenAI(apiKey, baseURL string, hc *http.Client) Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	return &openAIClient{client: openai.NewClient(opts...)}
}

func (o *openAIClient) Complete(ctx context.Context, req Request) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(req.Model),
		MaxCompletionTokens: openai.Int(req.MaxTokens),
	}
	if system := openAISystem(req.System); system != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(system))
	}
	params.Messages = append(params.Messages, openai.UserMessage(req.User))
	if req.Schema != nil {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   req.SchemaName,
					Schema: req.Schema,
					Strict: openai.Bool(true),
				},
			},
		}
	}

	resp, err := o.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", openAIError(err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("openai: reply had no choices")
	}
	choice := resp.Choices[0]
	if choice.Message.Refusal != "" {
		return "", ErrRefused
	}
	if choice.FinishReason == "length" {
		return "", ErrTruncated
	}
	return choice.Message.Content, nil
}

func (o *openAIClient) Chat(ctx context.Context, req ChatRequest, onText func(string)) (Reply, error) {
	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(req.Model),
		MaxCompletionTokens: openai.Int(req.MaxTokens),
	}
	if system := openAISystem(req.System); system != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(system))
	}
	for _, m := range req.Messages {
		params.Messages = append(params.Messages, openAIMessages(m)...)
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  shared.FunctionParameters(t.Schema),
			Strict:      openai.Bool(true),
		}))
	}

	stream := o.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	var acc openai.ChatCompletionAccumulator
	// Tool calls are put together here, not by the accumulator: it drops extra_content, and it
	// merges calls sent without an index, which is how Gemini sends them.
	var calls []ToolCall
	byIndex := map[int64]int{}
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			onText(delta.Content)
		}
		for _, d := range delta.ToolCalls {
			i := toolCallIndex(d, byIndex, len(calls))
			if i == len(calls) {
				calls = append(calls, ToolCall{})
			}
			c := &calls[i]
			if d.ID != "" {
				c.ID = d.ID
			}
			c.Name += d.Function.Name
			c.Input = append(c.Input, d.Function.Arguments...)
			if raw := d.JSON.ExtraFields["extra_content"].Raw(); raw != "" {
				c.Extra = json.RawMessage(raw)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return Reply{}, openAIError(err)
	}
	if len(acc.Choices) == 0 {
		return Reply{}, errors.New("openai: reply had no choices")
	}
	choice := acc.Choices[0]
	if choice.Message.Refusal != "" {
		return Reply{}, ErrRefused
	}
	if choice.FinishReason == "length" {
		return Reply{}, ErrTruncated
	}
	return Reply{Text: choice.Message.Content, ToolCalls: calls}, nil
}

// toolCallIndex is which call in calls a streamed piece belongs to, or len(calls) when it starts a
// new one. OpenAI numbers each call's pieces. Gemini sends each call whole and unnumbered.
func toolCallIndex(d openai.ChatCompletionChunkChoiceDeltaToolCall, byIndex map[int64]int, n int) int {
	if !d.JSON.Index.Valid() {
		return n
	}
	if i, ok := byIndex[d.Index]; ok {
		return i
	}
	byIndex[d.Index] = n
	return n
}

// openAIError adds the response body to an API error, whose message is only the status. The body
// says what was wrong, and some compatible servers send one the SDK can't parse.
func openAIError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		if body, readErr := io.ReadAll(apiErr.Response.Body); readErr == nil && len(body) > 0 {
			return fmt.Errorf("openai: %w: %s", err, body)
		}
	}
	return fmt.Errorf("openai: %w", err)
}

// openAISystem joins the layers in order, since chat completions take one system message.
func openAISystem(blocks []Block) string {
	layers := make([]string, len(blocks))
	for i, b := range blocks {
		layers[i] = b.Text
	}
	return strings.Join(layers, "\n\n")
}

// openAIMessages converts one message. Tool results become one tool message each.
func openAIMessages(m Message) []openai.ChatCompletionMessageParamUnion {
	if m.Role == RoleUser {
		var msgs []openai.ChatCompletionMessageParamUnion
		for _, r := range m.ToolResults {
			content := r.Content
			if r.IsError {
				content = "Error: " + content
			}
			msgs = append(msgs, openai.ToolMessage(content, r.CallID))
		}
		if m.Text != "" {
			msgs = append(msgs, openai.UserMessage(m.Text))
		}
		return msgs
	}
	assistant := openai.ChatCompletionAssistantMessageParam{}
	if m.Text != "" {
		assistant.Content.OfString = openai.String(m.Text)
	}
	for _, c := range m.ToolCalls {
		call := &openai.ChatCompletionMessageFunctionToolCallParam{
			ID:       c.ID,
			Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: c.Name, Arguments: string(c.Input)},
		}
		if c.Extra != nil {
			call.SetExtraFields(map[string]any{"extra_content": c.Extra})
		}
		assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{OfFunction: call})
	}
	return []openai.ChatCompletionMessageParamUnion{{OfAssistant: &assistant}}
}
