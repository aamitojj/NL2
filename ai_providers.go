package main

// AI provider adapters. Formats were checked against the official docs
// (October 2026), see docs/AUTOMATION.md "Assistant providers":
//   - OpenAI-compatible Chat Completions (OpenAI, xAI, OpenRouter, Mistral,
//     Groq, DeepSeek, local servers): POST {base}/chat/completions, stream:true,
//     SSE "data: {choices[0].delta{content,tool_calls[]}}" ... "data: [DONE]";
//     GET {base}/models -> {data:[{id}]}.
//   - Anthropic Messages: POST https://api.anthropic.com/v1/messages with
//     x-api-key + anthropic-version: 2023-06-01; SSE events message_start,
//     content_block_start/delta/stop (text_delta, input_json_delta),
//     message_delta, message_stop, ping, error. GET /v1/models.
//   - Gemini native: POST {base}/models/{model}:streamGenerateContent?alt=sse
//     with x-goog-api-key; contents[{role:user|model, parts[{text}]}],
//     systemInstruction, tools[{functionDeclarations}]; parts may carry
//     functionCall{name,args}. GET /v1beta/models.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

type ChatMsg struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

type ToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type ChatRequest struct {
	Model    string
	System   string
	Messages []ChatMsg
	Tools    []ToolDef // nil = no native tools (text protocol only)
}

// StreamEvent is what adapters emit while streaming.
type StreamEvent struct {
	Text string    `json:"text,omitempty"`
	Tool *ToolCall `json:"tool,omitempty"`
}

type Adapter interface {
	Stream(ctx context.Context, req ChatRequest, emit func(StreamEvent)) error
	Models(ctx context.Context) ([]string, error)
}

// ProviderInfo describes a provider in the catalog.
type ProviderInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Company     string `json:"company"`
	Kind        string `json:"kind"` // openai | anthropic | gemini
	BaseURL     string `json:"base_url"`
	KeyURL      string `json:"key_url"`
	Cloud       bool   `json:"cloud"`
	NativeTools bool   `json:"native_tools"`
	Color       string `json:"color"`
	Hint        string `json:"hint"` // suggested default model
	Custom      bool   `json:"custom,omitempty"`
}

var cloudProviders = []ProviderInfo{
	{ID: "openai", Name: "ChatGPT (OpenAI)", Company: "OpenAI", Kind: "openai", BaseURL: "https://api.openai.com/v1", KeyURL: "https://platform.openai.com/api-keys", Cloud: true, NativeTools: true, Color: "#10a37f", Hint: "gpt-4o-mini"},
	{ID: "anthropic", Name: "Claude (Anthropic)", Company: "Anthropic", Kind: "anthropic", BaseURL: "https://api.anthropic.com/v1", KeyURL: "https://console.anthropic.com/settings/keys", Cloud: true, NativeTools: true, Color: "#d97757", Hint: "claude-sonnet"},
	{ID: "gemini", Name: "Gemini (Google)", Company: "Google", Kind: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta", KeyURL: "https://aistudio.google.com/apikey", Cloud: true, NativeTools: true, Color: "#4285f4", Hint: "gemini-flash"},
	{ID: "xai", Name: "Grok (xAI)", Company: "xAI", Kind: "openai", BaseURL: "https://api.x.ai/v1", KeyURL: "https://console.x.ai/", Cloud: true, NativeTools: true, Color: "#111111", Hint: "grok"},
	{ID: "openrouter", Name: "OpenRouter", Company: "OpenRouter", Kind: "openai", BaseURL: "https://openrouter.ai/api/v1", KeyURL: "https://openrouter.ai/keys", Cloud: true, NativeTools: true, Color: "#6467f2", Hint: ""},
	{ID: "mistral", Name: "Mistral", Company: "Mistral AI", Kind: "openai", BaseURL: "https://api.mistral.ai/v1", KeyURL: "https://console.mistral.ai/api-keys", Cloud: true, NativeTools: true, Color: "#fa520f", Hint: "mistral-small-latest"},
	{ID: "groq", Name: "Groq", Company: "Groq", Kind: "openai", BaseURL: "https://api.groq.com/openai/v1", KeyURL: "https://console.groq.com/keys", Cloud: true, NativeTools: true, Color: "#f55036", Hint: ""},
	{ID: "deepseek", Name: "DeepSeek", Company: "DeepSeek", Kind: "openai", BaseURL: "https://api.deepseek.com/v1", KeyURL: "https://platform.deepseek.com/api_keys", Cloud: true, NativeTools: true, Color: "#4d6bfe", Hint: "deepseek-chat"},
	{ID: "custom", Name: "Custom (OpenAI-compatible)", Company: "the server you entered", Kind: "openai", BaseURL: "", KeyURL: "", Cloud: true, NativeTools: false, Color: "#64748b", Custom: true},
}

func providerByID(id string) (ProviderInfo, bool) {
	for _, p := range cloudProviders {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderInfo{}, false
}

// ---------- errors ----------

// AIError is a friendly, user-facing provider error.
type AIError struct {
	Kind    string `json:"kind"` // auth | quota | rate | overloaded | network | bad_request | other
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

func (e *AIError) Error() string { return e.Message }

func mapAIError(provider string, status int, body []byte, netErr error) *AIError {
	if netErr != nil {
		if errors.Is(netErr, context.Canceled) {
			return &AIError{Kind: "canceled", Message: "Stopped."}
		}
		return &AIError{Kind: "network", Message: fmt.Sprintf("Cannot reach %s. Check your internet connection (or the address, for local AI) and try again.", provider)}
	}
	detail := providerErrorText(body)
	low := strings.ToLower(detail)
	e := &AIError{Status: status}
	switch {
	case status == 401 || status == 403 || strings.Contains(low, "invalid api key") || strings.Contains(low, "api key not valid"):
		e.Kind, e.Message = "auth", fmt.Sprintf("%s rejected the API key. Check that you pasted the whole key and that it is still active (Settings > Connect AI).", provider)
	case status == 402 || strings.Contains(low, "insufficient_quota") || strings.Contains(low, "credit") || strings.Contains(low, "billing"):
		e.Kind, e.Message = "quota", fmt.Sprintf("Your %s account has no credit or quota left. Add billing/credits in their console, or switch to another model.", provider)
	case status == 429:
		e.Kind, e.Message = "rate", fmt.Sprintf("%s says you are sending too many requests or reached your quota. Wait a minute and try again, or pick another model.", provider)
	case status == 529 || status == 503 || status == 502 || status == 500:
		e.Kind, e.Message = "overloaded", fmt.Sprintf("%s is busy or having problems right now. Try again in a moment.", provider)
	case status == 404:
		e.Kind, e.Message = "bad_request", fmt.Sprintf("%s does not know this model. Pick another model in Settings > Connect AI.", provider)
	case status >= 400:
		e.Kind, e.Message = "bad_request", fmt.Sprintf("%s could not handle the request.", provider)
	default:
		e.Kind, e.Message = "other", fmt.Sprintf("Unexpected answer from %s.", provider)
	}
	if detail != "" {
		if len(detail) > 240 {
			detail = detail[:240] + "..."
		}
		e.Message += " (" + detail + ")"
	}
	return e
}

// providerErrorText extracts a message from the common error shapes:
// {"error":{"message":..}}, {"error":"..."}, [{"error":{...}}] (Gemini),
// {"detail":"..."} (Mistral), {"type":"error","error":{"message"}} (Anthropic).
func providerErrorText(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	if b[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
			b = arr[0]
		}
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		s := string(b)
		if len(s) > 200 {
			s = s[:200]
		}
		return strings.TrimSpace(s)
	}
	switch e := m["error"].(type) {
	case string:
		return e
	case map[string]any:
		if s, ok := e["message"].(string); ok {
			return s
		}
	}
	if s, ok := m["detail"].(string); ok {
		return s
	}
	if s, ok := m["message"].(string); ok {
		return s
	}
	return ""
}

var aiHTTP = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 120 * time.Second,
}}

func aiDo(ctx context.Context, provider, method, url string, headers map[string]string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, &AIError{Kind: "bad_request", Message: "Invalid address: " + err.Error()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := aiHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, mapAIError(provider, 0, nil, ctx.Err())
		}
		return nil, mapAIError(provider, 0, nil, err)
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, mapAIError(provider, resp.StatusCode, b, nil)
	}
	return resp, nil
}

// readSSE calls fn(event, data) for every server-sent event.
func readSSE(r io.Reader, fn func(event, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var ev string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			ev = ""
			return nil
		}
		d := strings.Join(data, "\n")
		e := ev
		data = nil
		ev = ""
		return fn(e, d)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			ev = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return sc.Err()
}

// ---------- OpenAI-compatible ----------

type openAIAdapter struct {
	name, base, key string
	extra           map[string]string
}

func (a *openAIAdapter) headers() map[string]string {
	h := map[string]string{}
	if a.key != "" {
		h["Authorization"] = "Bearer " + a.key
	}
	for k, v := range a.extra {
		h[k] = v
	}
	return h
}

func (a *openAIAdapter) Models(ctx context.Context) ([]string, error) {
	resp, err := aiDo(ctx, a.name, "GET", strings.TrimRight(a.base, "/")+"/models", a.headers(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct { // Ollama /api/tags shape, just in case
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, mapAIError(a.name, 200, nil, nil)
	}
	var out []string
	for _, d := range r.Data {
		out = append(out, d.ID)
	}
	for _, d := range r.Models {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out, nil
}

func (a *openAIAdapter) Stream(ctx context.Context, req ChatRequest, emit func(StreamEvent)) error {
	msgs := []map[string]any{}
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": req.Model, "messages": msgs, "stream": true}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
		}
		body["tools"] = tools
	}
	resp, err := aiDo(ctx, a.name, "POST", strings.TrimRight(a.base, "/")+"/chat/completions", a.headers(), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	type tc struct {
		name string
		args strings.Builder
	}
	calls := map[int]*tc{}
	var order []int
	err = readSSE(resp.Body, func(_, data string) error {
		if data == "[DONE]" {
			return io.EOF
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			return mapAIError(a.name, 500, []byte(`{"error":`+string(ch.Error)+`}`), nil)
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" {
				emit(StreamEvent{Text: c.Delta.Content})
			}
			for _, t := range c.Delta.ToolCalls {
				x := calls[t.Index]
				if x == nil {
					x = &tc{}
					calls[t.Index] = x
					order = append(order, t.Index)
				}
				if t.Function.Name != "" {
					x.name = t.Function.Name
				}
				x.args.WriteString(t.Function.Arguments)
			}
		}
		return nil
	})
	if err != nil && err != io.EOF {
		if ctx.Err() != nil {
			return mapAIError(a.name, 0, nil, ctx.Err())
		}
		var ae *AIError
		if errors.As(err, &ae) {
			return ae
		}
		return mapAIError(a.name, 0, nil, err)
	}
	for _, i := range order {
		emitTool(emit, calls[i].name, calls[i].args.String())
	}
	return nil
}

func emitTool(emit func(StreamEvent), name, args string) {
	if name == "" {
		return
	}
	m := map[string]any{}
	if strings.TrimSpace(args) != "" {
		_ = json.Unmarshal([]byte(args), &m)
	}
	emit(StreamEvent{Tool: &ToolCall{Name: name, Args: m}})
}

// ---------- Anthropic ----------

type anthropicAdapter struct{ base, key string }

const anthropicVersion = "2023-06-01"

func (a *anthropicAdapter) headers() map[string]string {
	return map[string]string{"x-api-key": a.key, "anthropic-version": anthropicVersion}
}

func (a *anthropicAdapter) Models(ctx context.Context) ([]string, error) {
	resp, err := aiDo(ctx, "Anthropic", "GET", strings.TrimRight(a.base, "/")+"/models?limit=100", a.headers(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, mapAIError("Anthropic", 200, nil, nil)
	}
	var out []string
	for _, d := range r.Data {
		out = append(out, d.ID)
	}
	return out, nil
}

// mergeTurns makes roles alternate and start with "user" (required by the
// Messages API and by Gemini).
func mergeTurns(in []ChatMsg) []ChatMsg {
	var out []ChatMsg
	for _, m := range in {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if m.Role != "assistant" {
			m.Role = "user"
		}
		if len(out) == 0 && m.Role == "assistant" {
			out = append(out, ChatMsg{Role: "user", Content: "(conversation continues)"})
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Content += "\n\n" + m.Content
			continue
		}
		out = append(out, m)
	}
	return out
}

func (a *anthropicAdapter) Stream(ctx context.Context, req ChatRequest, emit func(StreamEvent)) error {
	var msgs []map[string]any
	for _, m := range mergeTurns(req.Messages) {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": req.Model, "max_tokens": 2048, "messages": msgs, "stream": true}
	if req.System != "" {
		body["system"] = req.System
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters})
		}
		body["tools"] = tools
	}
	resp, err := aiDo(ctx, "Anthropic", "POST", strings.TrimRight(a.base, "/")+"/messages", a.headers(), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	type block struct {
		typ, name string
		json      strings.Builder
	}
	blocks := map[int]*block{}
	err = readSSE(resp.Body, func(event, data string) error {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "content_block_start":
			blocks[ev.Index] = &block{typ: ev.ContentBlock.Type, name: ev.ContentBlock.Name}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				emit(StreamEvent{Text: ev.Delta.Text})
			case "input_json_delta":
				if b := blocks[ev.Index]; b != nil {
					b.json.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if b := blocks[ev.Index]; b != nil && b.typ == "tool_use" {
				emitTool(emit, b.name, b.json.String())
			}
		case "message_stop":
			return io.EOF
		case "error":
			st := 500
			switch ev.Error.Type {
			case "overloaded_error":
				st = 529
			case "rate_limit_error":
				st = 429
			case "authentication_error":
				st = 401
			}
			return mapAIError("Anthropic", st, []byte(data), nil)
		}
		return nil
	})
	return finishStreamErr("Anthropic", ctx, err)
}

func finishStreamErr(name string, ctx context.Context, err error) error {
	if err == nil || err == io.EOF {
		return nil
	}
	if ctx.Err() != nil {
		return mapAIError(name, 0, nil, ctx.Err())
	}
	var ae *AIError
	if errors.As(err, &ae) {
		return ae
	}
	return mapAIError(name, 0, nil, err)
}

// ---------- Gemini (native API) ----------

type geminiAdapter struct{ base, key string }

func (a *geminiAdapter) Models(ctx context.Context) ([]string, error) {
	resp, err := aiDo(ctx, "Google Gemini", "GET", strings.TrimRight(a.base, "/")+"/models?pageSize=1000", map[string]string{"x-goog-api-key": a.key}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, mapAIError("Google Gemini", 200, nil, nil)
	}
	var out []string
	for _, m := range r.Models {
		ok := len(m.Methods) == 0
		for _, x := range m.Methods {
			if x == "generateContent" || x == "streamGenerateContent" {
				ok = true
			}
		}
		if ok {
			out = append(out, strings.TrimPrefix(m.Name, "models/"))
		}
	}
	return out, nil
}

func (a *geminiAdapter) Stream(ctx context.Context, req ChatRequest, emit func(StreamEvent)) error {
	var contents []map[string]any
	for _, m := range mergeTurns(req.Messages) {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]any{{"text": m.Content}}})
	}
	body := map[string]any{"contents": contents}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": req.System}}}
	}
	if len(req.Tools) > 0 {
		var decl []map[string]any
		for _, t := range req.Tools {
			decl = append(decl, map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decl}}
	}
	model := strings.TrimPrefix(req.Model, "models/")
	u := strings.TrimRight(a.base, "/") + "/models/" + model + ":streamGenerateContent?alt=sse"
	resp, err := aiDo(ctx, "Google Gemini", "POST", u, map[string]string{"x-goog-api-key": a.key}, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	err = readSSE(resp.Body, func(_, data string) error {
		var r struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						Thought      bool   `json:"thought"`
						FunctionCall *struct {
							Name string         `json:"name"`
							Args map[string]any `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &r) != nil {
			return nil
		}
		if len(r.Error) > 0 {
			return mapAIError("Google Gemini", 500, []byte(data), nil)
		}
		for _, c := range r.Candidates {
			for _, p := range c.Content.Parts {
				if p.FunctionCall != nil {
					args := p.FunctionCall.Args
					if args == nil {
						args = map[string]any{}
					}
					emit(StreamEvent{Tool: &ToolCall{Name: p.FunctionCall.Name, Args: args}})
				} else if p.Text != "" && !p.Thought {
					emit(StreamEvent{Text: p.Text})
				}
			}
		}
		return nil
	})
	return finishStreamErr("Google Gemini", ctx, err)
}
