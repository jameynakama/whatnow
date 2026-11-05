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
	"time"

	"github.com/jameynakama/whatnow/internal/llm"
)

// fakeAPI serves one canned reply and records the request body it received.
type fakeAPI struct {
	status int
	reply  string
	path   string
	header http.Header
	body   map[string]any
}

func (f *fakeAPI) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path = r.URL.Path
		f.header = r.Header
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &f.body); err != nil {
			t.Errorf("request body is not JSON: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		io.WriteString(w, f.reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

var schema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
	"required":             []string{"ok"},
	"additionalProperties": false,
}

func request() llm.Request {
	return llm.Request{
		Model:      "test-model",
		System:     []llm.Block{{Text: "core"}, {Text: "about me", Cache: true}, {Text: "context"}},
		User:       "do FLEX1-666 today",
		Schema:     schema,
		SchemaName: "result",
		MaxTokens:  512,
	}
}

func anthropicReply(stopReason, text string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "test-model",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": stopReason,
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	return string(b)
}

func TestAnthropicRequestAndReply(t *testing.T) {
	api := &fakeAPI{status: 200, reply: anthropicReply("end_turn", `{"ok":true}`)}
	client := llm.NewAnthropic("sk-test", api.start(t), nil)

	got, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"ok":true}` {
		t.Errorf("reply = %q", got)
	}

	if api.path != "/v1/messages" || api.body["model"] != "test-model" || api.body["max_tokens"] != float64(512) {
		t.Errorf("unexpected request: path %s, body %v", api.path, api.body)
	}
	system := api.body["system"].([]any)
	if len(system) != 3 {
		t.Fatalf("system has %d blocks, want 3", len(system))
	}
	for i, wantCache := range []bool{false, true, false} {
		_, hasCache := system[i].(map[string]any)["cache_control"]
		if hasCache != wantCache {
			t.Errorf("system[%d] cache_control present = %v, want %v", i, hasCache, wantCache)
		}
	}
	format := api.body["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Errorf("output_config.format = %v", format)
	}
}

func TestAnthropicFallbackOptIn(t *testing.T) {
	cases := []struct {
		name     string
		fallback bool
	}{{"off", false}, {"on", true}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeAPI{status: 200, reply: anthropicReply("end_turn", `{"ok":true}`)}
			req := request()
			req.Fallback = c.fallback
			if _, err := llm.NewAnthropic("sk-test", api.start(t), nil).Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			beta := strings.Contains(api.header.Get("anthropic-beta"), "server-side-fallback-2026-07-01")
			fallbacks, sent := api.body["fallbacks"]
			if beta != c.fallback || sent != c.fallback || (sent && fallbacks != "default") {
				t.Errorf("beta header %v, fallbacks %v (sent %v); want both only when opted in", beta, fallbacks, sent)
			}
		})
	}
}

func TestOpenAIRequestAndReply(t *testing.T) {
	reply, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "test-model",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": `{"ok":true}`, "refusal": nil},
		}},
	})
	api := &fakeAPI{status: 200, reply: string(reply)}
	client := llm.NewOpenAI("sk-test", api.start(t), nil)

	got, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"ok":true}` {
		t.Errorf("reply = %q", got)
	}

	if api.path != "/chat/completions" {
		t.Errorf("path = %s", api.path)
	}
	messages := api.body["messages"].([]any)
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "core\n\nabout me\n\ncontext" {
		t.Errorf("system message = %v", system)
	}
	format := api.body["response_format"].(map[string]any)
	jsonSchema := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || jsonSchema["name"] != "result" || jsonSchema["strict"] != true {
		t.Errorf("response_format = %v", format)
	}
}

func TestOpenAIIgnoresTheEnvironmentBaseURL(t *testing.T) {
	api := &fakeAPI{status: 200, reply: "{}"}
	t.Setenv("OPENAI_BASE_URL", api.start(t))
	// An empty base URL must still mean OpenAI, never the environment: hosted mode shares the
	// process. The request goes to api.openai.com instead — a fast 401 online, a dial error off;
	// what matters is the env-named server never sees it.
	client := llm.NewOpenAI("sk-test", "", &http.Client{Timeout: 2 * time.Second})
	_, _ = client.Complete(context.Background(), llm.Request{Model: "m", User: "hi", MaxTokens: 16})
	if api.path != "" {
		t.Error("the request went to OPENAI_BASE_URL's server")
	}
}

func TestStopReasonsBecomeErrors(t *testing.T) {
	openAIReply := func(finish, refusal string) string {
		var r any
		if refusal != "" {
			r = refusal
		}
		b, _ := json.Marshal(map[string]any{
			"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": finish,
				"message": map[string]any{"role": "assistant", "content": "", "refusal": r}}},
		})
		return string(b)
	}
	cases := []struct {
		name   string
		client func(url string) llm.Client
		reply  string
		want   error
	}{
		{"anthropic refusal", func(u string) llm.Client { return llm.NewAnthropic("k", u, nil) }, anthropicReply("refusal", ""), llm.ErrRefused},
		{"anthropic max tokens", func(u string) llm.Client { return llm.NewAnthropic("k", u, nil) }, anthropicReply("max_tokens", `{"ok":`), llm.ErrTruncated},
		{"openai refusal", func(u string) llm.Client { return llm.NewOpenAI("k", u, nil) }, openAIReply("stop", "I can't help with that"), llm.ErrRefused},
		{"openai length", func(u string) llm.Client { return llm.NewOpenAI("k", u, nil) }, openAIReply("length", ""), llm.ErrTruncated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeAPI{status: 200, reply: c.reply}
			_, err := c.client(api.start(t)).Complete(context.Background(), request())
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestAuthErrorDoesNotLeakTheKey(t *testing.T) {
	const key = "sk-very-secret-123"
	body := `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	for name, newClient := range map[string]func(string, string, *http.Client) llm.Client{"anthropic": llm.NewAnthropic, "openai": llm.NewOpenAI} {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{status: 401, reply: body}
			_, err := newClient(key, api.start(t), nil).Complete(context.Background(), request())
			if err == nil {
				t.Fatal("expected an error for a 401")
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("error message contains the API key: %v", err)
			}
		})
	}
}

func TestOpenAISkipsAnEmptySystemMessage(t *testing.T) {
	reply, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "ok", "refusal": nil}}},
	})
	api := &fakeAPI{status: 200, reply: string(reply)}

	_, err := llm.NewOpenAI("k", api.start(t), nil).Complete(context.Background(), llm.Request{Model: "m", User: "hi", MaxTokens: 16})
	if err != nil {
		t.Fatal(err)
	}
	messages := api.body["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Errorf("messages = %v, want only the user message", messages)
	}
	if _, ok := api.body["response_format"]; ok {
		t.Error("response_format sent without a schema")
	}
}

func TestAnthropicWithOwnHTTPClientIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "server-token")
	api := &fakeAPI{status: 200, reply: anthropicReply("end_turn", `{"ok":true}`)}

	if _, err := llm.NewAnthropic("sk-user", api.start(t), &http.Client{}).Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if got := api.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want the server's token kept out", got)
	}
	if got := api.header.Get("X-Api-Key"); got != "sk-user" {
		t.Errorf("X-Api-Key = %q, want the user's key", got)
	}
}
