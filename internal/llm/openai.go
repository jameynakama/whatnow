package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type openAIClient struct {
	client openai.Client
}

// defaultBaseURL is OpenAI's endpoint, what the SDK itself uses for production. NewOpenAI always
// sets the base URL explicitly, down to this default: the SDK has no WithoutEnvironmentDefaults
// (as of v3.71.2 the marker is internal-only), so without an explicit one OPENAI_BASE_URL in the
// process environment would name the endpoint. Hosted mode also refuses OPENAI_* variables at
// startup; pinning here keeps a hosted user's requests on their own settings either way. The API
// key needs no such guard: WithAPIKey is always explicit, so OPENAI_API_KEY never applies.
const defaultBaseURL = "https://api.openai.com/v1/"

// NewOpenAI returns a Client for the OpenAI chat completions API or any server compatible with
// it, such as Ollama, LM Studio, or OpenRouter. An empty baseURL uses OpenAI, and a nil hc the SDK's HTTP client.
func NewOpenAI(apiKey, baseURL string, hc *http.Client) Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)}
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
		return "", fmt.Errorf("openai: %w", err)
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
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			onText(chunk.Choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		return Reply{}, fmt.Errorf("openai: %w", err)
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
	reply := Reply{Text: choice.Message.Content}
	for _, c := range choice.Message.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Input: json.RawMessage(c.Function.Arguments)})
	}
	return reply, nil
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
		assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID:       c.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: c.Name, Arguments: string(c.Input)},
			},
		})
	}
	return []openai.ChatCompletionMessageParamUnion{{OfAssistant: &assistant}}
}
