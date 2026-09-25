package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/kgatilin/muninn/internal/gcp"
	"github.com/kgatilin/muninn/internal/usage"
)

const (
	DefaultOllamaEndpoint    = "http://localhost:11434"
	DefaultGeminiEndpoint    = "https://generativelanguage.googleapis.com"
	DefaultAnthropicEndpoint = "https://api.anthropic.com"

	anthropicVersion = "2023-06-01"
	// maxOutput bounds an answer. The answers asked for here are a few hundred
	// tokens; the room is for a model that reasons before it answers.
	maxOutput = 4096
)

func endpoint(s Spec, def string) string {
	if s.Endpoint != "" {
		return strings.TrimRight(s.Endpoint, "/")
	}
	return def
}

// ollama is /api/chat with the schema as `format`, which constrains decoding.
// Thinking is switched off: naming entities does not need it, and a small
// reasoning model spends minutes on a passage before its first token of the
// answer. A model that has no thinking refuses the field, and is then asked
// without it.
type ollama struct {
	spec    Spec
	noThink atomic.Bool // the model refused `think`
}

func newOllama(s Spec) *ollama { return &ollama{spec: s} }

func (o *ollama) ID() string { return o.spec.String() }

func (o *ollama) Complete(ctx context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	body := map[string]any{
		"model":  o.spec.Model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"options": map[string]any{"temperature": 0, "num_predict": maxOutput},
	}
	if len(schema) > 0 {
		body["format"] = schema
	}
	if !o.noThink.Load() {
		body["think"] = false
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	url := endpoint(o.spec, DefaultOllamaEndpoint) + "/api/chat"
	err := postJSON(ctx, "ollama "+o.spec.Model, url, nil, body, &out)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusBadRequest && strings.Contains(se.body, "think") {
		o.noThink.Store(true)
		delete(body, "think")
		err = postJSON(ctx, "ollama "+o.spec.Model, url, nil, body, &out)
	}
	if err != nil {
		return "", usage.Tokens{}, err
	}
	return out.Message.Content, usage.Tokens{}, nil
}

// gemini is the Generative Language API's generateContent, with the JSON mime
// type and the schema in generationConfig.
type gemini struct {
	spec Spec
	key  string
}

func newGemini(s Spec, key string) *gemini { return &gemini{s, key} }

func (g *gemini) ID() string { return g.spec.String() }

func (g *gemini) Complete(ctx context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	url := endpoint(g.spec, DefaultGeminiEndpoint) + "/v1beta/models/" + g.spec.Model + ":generateContent"
	return generateContent(ctx, "gemini "+g.spec.Model, url, map[string]string{"x-goog-api-key": g.key}, system, user, schema)
}

// vertex is the same generateContent on Vertex AI: the project and location
// come from the environment of the run, the bearer token from the application
// default credentials.
type vertex struct{ spec Spec }

func newVertex(s Spec) *vertex { return &vertex{s} }

func (v *vertex) ID() string { return v.spec.String() }

func (v *vertex) Complete(ctx context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	url, err := gcp.ModelURL(v.spec.Endpoint, v.spec.Model, "generateContent")
	if err != nil {
		return "", usage.Tokens{}, fmt.Errorf("vertex: %w", err)
	}
	token, err := gcp.Token(ctx)
	if err != nil {
		return "", usage.Tokens{}, fmt.Errorf("vertex: %w", err)
	}
	return generateContent(ctx, "vertex "+v.spec.Model, url, map[string]string{"Authorization": "Bearer " + token}, system, user, schema)
}

func generateContent(ctx context.Context, name, url string, header map[string]string, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	config := map[string]any{"temperature": 0, "maxOutputTokens": maxOutput, "responseMimeType": "application/json"}
	if len(schema) > 0 {
		config["responseJsonSchema"] = schema
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]string{{"text": system}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]string{{"text": user}}}},
		"generationConfig":  config,
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Usage struct {
			Prompt     int `json:"promptTokenCount"`
			Candidates int `json:"candidatesTokenCount"`
			Thoughts   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := postJSON(ctx, name, url, header, body, &out); err != nil {
		return "", usage.Tokens{}, err
	}
	// Thinking is billed as output.
	tokens := usage.Tokens{In: out.Usage.Prompt, Out: out.Usage.Candidates + out.Usage.Thoughts, Requests: 1}
	if len(out.Candidates) == 0 {
		return "", tokens, fmt.Errorf("%s: %w: no candidates (block reason %q)", name, ErrNoAnswer, out.PromptFeedback.BlockReason)
	}
	var text strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	return text.String(), tokens, nil
}

// anthropic is the Messages API with a structured output: output_config.format
// holds the schema, and the answer is the text blocks of the response.
type anthropic struct {
	spec Spec
	key  string
}

func newAnthropic(s Spec, key string) *anthropic { return &anthropic{s, key} }

func (a *anthropic) ID() string { return a.spec.String() }

func (a *anthropic) Complete(ctx context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	body := map[string]any{
		"model":      a.spec.Model,
		"max_tokens": maxOutput,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	}
	if len(schema) > 0 {
		body["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	header := map[string]string{"x-api-key": a.key, "anthropic-version": anthropicVersion}
	if err := postJSON(ctx, "anthropic "+a.spec.Model, endpoint(a.spec, DefaultAnthropicEndpoint)+"/v1/messages", header, body, &out); err != nil {
		return "", usage.Tokens{}, err
	}
	tokens := usage.Tokens{In: out.Usage.In, Out: out.Usage.Out, Requests: 1}
	// A refusal or a cut answer does not follow the schema; the caller's
	// validation would say so less clearly.
	if out.StopReason == "refusal" || out.StopReason == "max_tokens" {
		return "", tokens, fmt.Errorf("anthropic %s: %w: stop_reason %s", a.spec.Model, ErrNoAnswer, out.StopReason)
	}
	var text strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return text.String(), tokens, nil
}
