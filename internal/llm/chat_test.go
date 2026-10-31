package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jameynakama/whatnow/internal/llm"
)

// sseAPI streams canned server-sent events and records the request body it received.
type sseAPI struct {
	events []string
	body   map[string]any
}

func (f *sseAPI) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &f.body); err != nil {
			t.Errorf("request body is not JSON: %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range f.events {
			io.WriteString(w, e+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func anthropicEvent(v map[string]any) string {
	b, _ := json.Marshal(v)
	return "event: " + v["type"].(string) + "\ndata: " + string(b)
}

func anthropicStream(stopReason string, blocks ...[]map[string]any) []string {
	events := []string{anthropicEvent(map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "test-model", "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})}
	for i, block := range blocks {
		events = append(events, anthropicEvent(map[string]any{"type": "content_block_start", "index": i, "content_block": block[0]}))
		for _, delta := range block[1:] {
			events = append(events, anthropicEvent(map[string]any{"type": "content_block_delta", "index": i, "delta": delta}))
		}
		events = append(events, anthropicEvent(map[string]any{"type": "content_block_stop", "index": i}))
	}
	return append(events,
		anthropicEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason},
			"usage": map[string]any{"output_tokens": 5}}),
		anthropicEvent(map[string]any{"type": "message_stop"}))
}

func openAIChunk(delta map[string]any, finish any) string {
	b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}})
	return "data: " + string(b)
}

var completeTask = llm.Tool{Name: "complete_task", Description: "Mark a task done.", Schema: map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"task_id": map[string]any{"type": "integer"}},
	"required":             []string{"task_id"},
	"additionalProperties": false,
}}

// chatRequest is a turn mid-conversation: the model called a tool, got its result, and goes on.
func chatRequest() llm.ChatRequest {
	return llm.ChatRequest{
		Model:  "test-model",
		System: []llm.Block{{Text: "core", Cache: true}, {Text: "context"}},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "I finished the RFC"},
			{Role: llm.RoleAssistant, Text: "Nice.", ToolCalls: []llm.ToolCall{{ID: "call_0", Name: "complete_task", Input: json.RawMessage(`{"task_id":3}`)}}},
			{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{CallID: "call_0", Content: "no task #3", IsError: true}}},
		},
		Tools:     []llm.Tool{completeTask},
		MaxTokens: 512,
	}
}

func collect(t *testing.T, client llm.Client, req llm.ChatRequest) (llm.Reply, []string) {
	t.Helper()
	var deltas []string
	reply, err := client.Chat(context.Background(), req, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	return reply, deltas
}

func TestAnthropicChatStreamsTextAndToolCalls(t *testing.T) {
	api := &sseAPI{events: anthropicStream("tool_use",
		[]map[string]any{{"type": "text", "text": ""}, {"type": "text_delta", "text": "Let me "}, {"type": "text_delta", "text": "fix that."}},
		[]map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "complete_task", "input": map[string]any{}},
			{"type": "input_json_delta", "partial_json": `{"task_`}, {"type": "input_json_delta", "partial_json": `id": 4}`}},
	)}
	reply, deltas := collect(t, llm.NewAnthropic("sk-test", api.start(t), nil), chatRequest())

	want := llm.Reply{Text: "Let me fix that.", ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "complete_task", Input: json.RawMessage(`{"task_id": 4}`)}}}
	if diff := cmp.Diff(want, reply); diff != "" {
		t.Errorf("reply (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"Let me ", "fix that."}, deltas); diff != "" {
		t.Errorf("deltas (-want +got):\n%s", diff)
	}

	if api.body["stream"] != true {
		t.Errorf("stream = %v", api.body["stream"])
	}
	tool := api.body["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "complete_task" || tool["strict"] != true || schema["additionalProperties"] != false || schema["type"] != "object" {
		t.Errorf("tool = %v", tool)
	}
	msgs := api.body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want 3", len(msgs))
	}
	assistant := msgs[1].(map[string]any)["content"].([]any)
	use := assistant[1].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "call_0" || use["input"].(map[string]any)["task_id"] != float64(3) {
		t.Errorf("tool use = %v", use)
	}
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "call_0" || result["is_error"] != true {
		t.Errorf("tool result = %v", result)
	}
}

func TestOpenAIChatStreamsTextAndToolCalls(t *testing.T) {
	api := &sseAPI{events: []string{
		openAIChunk(map[string]any{"role": "assistant", "content": "Let me "}, nil),
		openAIChunk(map[string]any{"content": "fix that."}, nil),
		openAIChunk(map[string]any{"tool_calls": []map[string]any{{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "complete_task", "arguments": ""}}}}, nil),
		openAIChunk(map[string]any{"tool_calls": []map[string]any{{"index": 0, "function": map[string]any{"arguments": `{"task_id":4}`}}}}, nil),
		openAIChunk(map[string]any{}, "tool_calls"),
		"data: [DONE]",
	}}
	reply, deltas := collect(t, llm.NewOpenAI("sk-test", api.start(t), nil), chatRequest())

	want := llm.Reply{Text: "Let me fix that.", ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "complete_task", Input: json.RawMessage(`{"task_id":4}`)}}}
	if diff := cmp.Diff(want, reply); diff != "" {
		t.Errorf("reply (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"Let me ", "fix that."}, deltas); diff != "" {
		t.Errorf("deltas (-want +got):\n%s", diff)
	}

	tool := api.body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tool["name"] != "complete_task" || tool["strict"] != true {
		t.Errorf("tool = %v", tool)
	}
	msgs := api.body["messages"].([]any)
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.(map[string]any)["role"].(string)
	}
	if diff := cmp.Diff([]string{"system", "user", "assistant", "tool"}, roles); diff != "" {
		t.Errorf("roles (-want +got):\n%s", diff)
	}
	call := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_0" || call["function"].(map[string]any)["arguments"] != `{"task_id":3}` {
		t.Errorf("tool call = %v", call)
	}
	result := msgs[3].(map[string]any)
	if result["tool_call_id"] != "call_0" || result["content"] != "Error: no task #3" {
		t.Errorf("tool result = %v", result)
	}
}

// Gemini's OpenAI-compatible stream sends each tool call whole in its own chunk, with no index,
// and puts a thought signature on the first that must be sent back with it.
func TestOpenAIChatHandlesGeminiToolCalls(t *testing.T) {
	extra := `{"google":{"thought_signature":"sig=="}}`
	api := &sseAPI{events: []string{
		openAIChunk(map[string]any{"role": "assistant", "tool_calls": []map[string]any{{"id": "call_1", "type": "function",
			"extra_content": json.RawMessage(extra), "function": map[string]any{"name": "complete_task", "arguments": `{"task_id":4}`}}}}, nil),
		openAIChunk(map[string]any{"tool_calls": []map[string]any{{"id": "call_2", "type": "function",
			"function": map[string]any{"name": "complete_task", "arguments": `{"task_id":5}`}}}}, nil),
		openAIChunk(map[string]any{}, "tool_calls"),
		"data: [DONE]",
	}}
	client := llm.NewOpenAI("sk-test", api.start(t), nil)
	reply, _ := collect(t, client, chatRequest())
	want := llm.Reply{ToolCalls: []llm.ToolCall{
		{ID: "call_1", Name: "complete_task", Input: json.RawMessage(`{"task_id":4}`), Extra: json.RawMessage(extra)},
		{ID: "call_2", Name: "complete_task", Input: json.RawMessage(`{"task_id":5}`)},
	}}
	if diff := cmp.Diff(want, reply); diff != "" {
		t.Fatalf("reply (-want +got):\n%s", diff)
	}

	req := chatRequest()
	req.Messages[1].ToolCalls = reply.ToolCalls
	collect(t, client, req)
	calls := api.body["messages"].([]any)[2].(map[string]any)["tool_calls"].([]any)
	if diff := cmp.Diff(map[string]any{"google": map[string]any{"thought_signature": "sig=="}}, calls[0].(map[string]any)["extra_content"]); diff != "" {
		t.Errorf("echoed extra_content (-want +got):\n%s", diff)
	}
	if _, ok := calls[1].(map[string]any)["extra_content"]; ok {
		t.Errorf("second call got extra_content it didn't come with: %v", calls[1])
	}
}

func TestChatStopReasonsBecomeErrors(t *testing.T) {
	text := []map[string]any{{"type": "text", "text": ""}, {"type": "text_delta", "text": "Sure, here"}}
	cases := []struct {
		name   string
		client func(url string) llm.Client
		events []string
		want   error
	}{
		{"anthropic refusal", anthropicClient, anthropicStream("refusal", text), llm.ErrRefused},
		{"anthropic max tokens", anthropicClient, anthropicStream("max_tokens", text), llm.ErrTruncated},
		{"openai length", openAIClient, []string{openAIChunk(map[string]any{"content": "Sure, here"}, "length"), "data: [DONE]"}, llm.ErrTruncated},
		{"openai refusal", openAIClient, []string{openAIChunk(map[string]any{"refusal": "I can't"}, "stop"), "data: [DONE]"}, llm.ErrRefused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &sseAPI{events: c.events}
			_, err := c.client(api.start(t)).Chat(context.Background(), chatRequest(), func(string) {})
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestOpenAIErrorIncludesResponseBody(t *testing.T) {
	// Gemini's OpenAI-compatible endpoint wraps its error in an array, which the SDK can't parse.
	body := `[{"error":{"code":400,"message":"Function call is missing a thought_signature","status":"INVALID_ARGUMENT"}}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	client := openAIClient(srv.URL)
	_, chatErr := client.Chat(context.Background(), chatRequest(), func(string) {})
	_, completeErr := client.Complete(context.Background(), llm.Request{Model: "test-model", User: "hi", MaxTokens: 10})
	for name, err := range map[string]error{"chat": chatErr, "complete": completeErr} {
		if err == nil || !strings.Contains(err.Error(), "missing a thought_signature") {
			t.Errorf("%s err = %v, want the response body in it", name, err)
		}
	}
}

func TestChatProviderErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	t.Cleanup(srv.Close)
	for name, client := range map[string]llm.Client{"anthropic": anthropicClient(srv.URL), "openai": openAIClient(srv.URL)} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Chat(context.Background(), chatRequest(), func(string) {})
			if err == nil || !strings.Contains(err.Error(), "401") {
				t.Errorf("err = %v, want a 401", err)
			}
		})
	}
}

func anthropicClient(url string) llm.Client { return llm.NewAnthropic("sk-test", url, nil) }
func openAIClient(url string) llm.Client    { return llm.NewOpenAI("sk-test", url, nil) }
