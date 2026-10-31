// Package llm is the provider-neutral way to call a language model.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	// ErrRefused means the model declined the request.
	ErrRefused = errors.New("model declined the request")
	// ErrTruncated means the reply hit the token limit before it finished.
	ErrTruncated = errors.New("model reply was cut off")
)

// Client calls one provider. The Claude API and OpenAI-compatible APIs implement it.
type Client interface {
	Complete(ctx context.Context, req Request) (string, error)
	// Chat runs one model turn of a conversation that may call tools. It calls onText with each
	// piece of reply text as it arrives, and returns the whole reply once the model stops.
	Chat(ctx context.Context, req ChatRequest, onText func(string)) (Reply, error)
}

// Block is one layer of a system prompt. Cache marks the end of a prefix that stays the same
// across calls, so providers with prompt caching can reuse it.
type Block struct {
	Text  string
	Cache bool
}

type Request struct {
	Model  string
	System []Block
	User   string
	// Schema, when set, constrains the reply to JSON matching it. SchemaName labels the schema
	// for providers that require a name.
	Schema     map[string]any
	SchemaName string
	MaxTokens  int64
	// Fallback asks the provider to re-serve a refused request on another model. Only the Claude
	// API supports it, on its main-tier models; other providers ignore it.
	Fallback bool
}

// Role is who sent a chat message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one message of a chat. An assistant message can carry tool calls, and the user
// message after it carries their results.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ToolCall is the model asking to run a tool. Input is a JSON object matching the tool's schema.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
	// Extra is provider data the call came with that must be sent back with it, such as the
	// thought signature Gemini puts in an OpenAI-compatible tool call's extra_content.
	Extra json.RawMessage
}

// ToolResult answers one tool call. IsError tells the model the call failed and why.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Tool is a function the model can call. Schema is a JSON schema for its input, built with the
// jsonschema package so both providers accept it in strict mode.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

type ChatRequest struct {
	Model     string
	System    []Block
	Messages  []Message
	Tools     []Tool
	MaxTokens int64
	// Fallback is as for Request.
	Fallback bool
}

// Reply is what the model said in one turn. Tool calls, if any, wait for their results.
type Reply struct {
	Text      string
	ToolCalls []ToolCall
}
