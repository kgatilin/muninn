package llm

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

	"github.com/kgatilin/muninn/internal/gcp/gcptest"
	"github.com/kgatilin/muninn/internal/usage"
)

var schema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)

// fake serves the scripted responses in order and keeps what it was sent.
type fake struct {
	t         *testing.T
	responses []response
	requests  []map[string]any
	headers   []http.Header
	paths     []string
}

type response struct {
	code int
	body string
}

func (f *fake) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("request body: %v", err)
		}
		f.requests = append(f.requests, body)
		f.headers = append(f.headers, r.Header)
		f.paths = append(f.paths, r.URL.Path)
		if len(f.responses) == 0 {
			f.t.Error("a request past the script")
			w.WriteHeader(500)
			return
		}
		next := f.responses[0]
		f.responses = f.responses[1:]
		w.WriteHeader(next.code)
		io.WriteString(w, next.body)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, provider string, f *fake) Client {
	t.Helper()
	backoff = time.Millisecond
	t.Setenv("GEMINI_API_KEY", "g-key")
	t.Setenv("ANTHROPIC_API_KEY", "a-key")
	spec, err := ParseSpec(provider)
	if err != nil {
		t.Fatal(err)
	}
	spec.Endpoint = f.server().URL
	c, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// at walks a decoded request by keys and indexes.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: not an object at %v", path, p)
			}
			v = m[key]
		case int:
			l, ok := v.([]any)
			if !ok || key >= len(l) {
				t.Fatalf("%v: no element %v", path, p)
			}
			v = l[key]
		}
	}
	return v
}

func TestOllamaWire(t *testing.T) {
	f := &fake{t: t, responses: []response{{200, `{"message":{"role":"assistant","content":"{\"ok\":true}"}}`}}}
	c := newClient(t, "ollama", f)
	got, tokens, err := c.Complete(context.Background(), "sys", "usr", schema)
	if err != nil || got != `{"ok":true}` || tokens != (usage.Tokens{}) {
		t.Fatalf("%q %v %v", got, tokens, err)
	}
	if c.ID() != "ollama:qwen3:4b" || f.paths[0] != "/api/chat" {
		t.Fatalf("%s %v", c.ID(), f.paths)
	}
	req := f.requests[0]
	if req["model"] != "qwen3:4b" || req["stream"] != false ||
		at(t, req, "messages", 0, "role") != "system" || at(t, req, "messages", 0, "content") != "sys" ||
		at(t, req, "messages", 1, "role") != "user" || at(t, req, "messages", 1, "content") != "usr" ||
		at(t, req, "format", "type") != "object" || req["think"] != false {
		t.Fatalf("request: %v", req)
	}
}

func TestOllamaModelWithoutThinking(t *testing.T) {
	ok := `{"message":{"content":"{}"}}`
	f := &fake{t: t, responses: []response{{400, `{"error":"\"gemma3\" does not support thinking"}`}, {200, ok}, {200, ok}}}
	c := newClient(t, "ollama:gemma3", f)
	for range 2 {
		if got, _, err := c.Complete(context.Background(), "s", "u", schema); err != nil || got != "{}" {
			t.Fatalf("%q %v", got, err)
		}
	}
	if f.requests[0]["think"] != false || len(f.requests) != 3 {
		t.Fatalf("requests: %v", f.requests)
	}
	for _, req := range f.requests[1:] {
		if _, sent := req["think"]; sent {
			t.Fatalf("think sent again: %v", req)
		}
	}
}

func TestGeminiWire(t *testing.T) {
	f := &fake{t: t, responses: []response{{200, `{"candidates":[{"content":{"parts":[{"text":"{\"ok\":"},{"text":"true}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":5,"thoughtsTokenCount":7}}`}}}
	c := newClient(t, "gemini:gemini-x", f)
	got, tokens, err := c.Complete(context.Background(), "sys", "usr", schema)
	if err != nil || got != `{"ok":true}` || tokens != (usage.Tokens{In: 40, Out: 12, Requests: 1}) {
		t.Fatalf("%q %v", got, err)
	}
	if f.paths[0] != "/v1beta/models/gemini-x:generateContent" || f.headers[0].Get("x-goog-api-key") != "g-key" {
		t.Fatalf("%v %v", f.paths, f.headers[0])
	}
	req := f.requests[0]
	if at(t, req, "systemInstruction", "parts", 0, "text") != "sys" ||
		at(t, req, "contents", 0, "role") != "user" || at(t, req, "contents", 0, "parts", 0, "text") != "usr" ||
		at(t, req, "generationConfig", "responseMimeType") != "application/json" ||
		at(t, req, "generationConfig", "responseJsonSchema", "type") != "object" {
		t.Fatalf("request: %v", req)
	}
}

func TestGeminiBlocked(t *testing.T) {
	f := &fake{t: t, responses: []response{{200, `{"promptFeedback":{"blockReason":"SAFETY"}}`}}}
	if _, _, err := newClient(t, "gemini", f).Complete(context.Background(), "s", "u", schema); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a blocked prompt: %v", err)
	}
}

func TestAnthropicWire(t *testing.T) {
	f := &fake{t: t, responses: []response{{200, `{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":9}}`}}}
	c := newClient(t, "anthropic", f)
	got, tokens, err := c.Complete(context.Background(), "sys", "usr", schema)
	if err != nil || got != `{"ok":true}` || tokens != (usage.Tokens{In: 30, Out: 9, Requests: 1}) {
		t.Fatalf("%q %v", got, err)
	}
	h := f.headers[0]
	if f.paths[0] != "/v1/messages" || h.Get("x-api-key") != "a-key" || h.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("%v %v", f.paths, h)
	}
	req := f.requests[0]
	if req["model"] != "claude-haiku-4-5" || req["system"] != "sys" || req["max_tokens"] == nil ||
		at(t, req, "messages", 0, "role") != "user" || at(t, req, "messages", 0, "content") != "usr" ||
		at(t, req, "output_config", "format", "type") != "json_schema" ||
		at(t, req, "output_config", "format", "schema", "additionalProperties") != false {
		t.Fatalf("request: %v", req)
	}
}

func TestAnthropicRefusal(t *testing.T) {
	f := &fake{t: t, responses: []response{{200, `{"content":[],"stop_reason":"refusal"}`}}}
	if _, _, err := newClient(t, "anthropic", f).Complete(context.Background(), "s", "u", schema); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a refusal: %v", err)
	}
}

func TestRetries(t *testing.T) {
	ok := map[string]string{
		"ollama":    `{"message":{"content":"{}"}}`,
		"gemini":    `{"candidates":[{"content":{"parts":[{"text":"{}"}]}}]}`,
		"anthropic": `{"content":[{"type":"text","text":"{}"}]}`,
	}
	for provider, body := range ok {
		t.Run(provider, func(t *testing.T) {
			f := &fake{t: t, responses: []response{{429, "slow down"}, {503, "busy"}, {200, body}}}
			got, _, err := newClient(t, provider, f).Complete(context.Background(), "s", "u", schema)
			if err != nil || got != "{}" || len(f.requests) != 3 {
				t.Fatalf("%q %v after %d requests", got, err, len(f.requests))
			}
		})
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	f := &fake{t: t, responses: []response{{400, "bad schema"}}}
	_, _, err := newClient(t, "anthropic", f).Complete(context.Background(), "s", "u", schema)
	if err == nil || !strings.Contains(err.Error(), "bad schema") || len(f.requests) != 1 {
		t.Fatalf("%v after %d requests", err, len(f.requests))
	}
}

func TestRetriesAreBounded(t *testing.T) {
	f := &fake{t: t, responses: []response{{500, ""}, {500, ""}, {500, ""}, {500, ""}}}
	if _, _, err := newClient(t, "ollama", f).Complete(context.Background(), "s", "u", schema); err == nil || len(f.requests) != attempts {
		t.Fatalf("%v after %d requests", err, len(f.requests))
	}
}

func TestMalformedEnvelope(t *testing.T) {
	f := &fake{t: t, responses: []response{{200, `{"message": not json`}, {200, `{"message": not json`}, {200, `{"message": not json`}, {200, `{"message": not json`}}}
	if _, _, err := newClient(t, "ollama", f).Complete(context.Background(), "s", "u", schema); err == nil {
		t.Fatal("a response that is not JSON was accepted")
	}
}

func TestCancelStopsTheWait(t *testing.T) {
	f := &fake{t: t, responses: []response{{429, ""}}}
	c := newClient(t, "ollama", f)
	backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.Complete(ctx, "s", "u", schema); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestMissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	spec, _ := ParseSpec("anthropic")
	if _, err := New(spec); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("%v", err)
	}
	if _, err := ParseSpec("openai:gpt"); err == nil {
		t.Fatal("an unknown provider parsed")
	}
}

func TestVertexWire(t *testing.T) {
	gcptest.Setup(t)
	f := &fake{t: t, responses: []response{{200, `{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":21,"candidatesTokenCount":4}}`}}}
	c := newClient(t, "vertex", f)
	got, tokens, err := c.Complete(context.Background(), "sys", "usr", schema)
	if err != nil || got != `{"ok":true}` || tokens != (usage.Tokens{In: 21, Out: 4, Requests: 1}) {
		t.Fatalf("%q %v", got, err)
	}
	want := "/v1/projects/" + gcptest.Project + "/locations/global/publishers/google/models/gemini-3.5-flash-lite:generateContent"
	if f.paths[0] != want || f.headers[0].Get("Authorization") != "Bearer "+gcptest.Token {
		t.Fatalf("%v %v", f.paths, f.headers[0])
	}
	if at(t, f.requests[0], "generationConfig", "responseJsonSchema", "type") != "object" {
		t.Fatalf("request: %v", f.requests[0])
	}
}
