package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kgatilin/muninn/internal/usage"
)

// The OpenAI embeddings API, and any server that speaks it: `embedder.endpoint`
// points elsewhere. The key is read from the environment when the embedder is
// first used and is never written anywhere, so a bank on this provider can be
// made, shown and searched lexically without one.

const (
	DefaultOpenAIEndpoint = "https://api.openai.com"
	DefaultOpenAIModel    = "text-embedding-3-small"
	openAIKeyEnv          = "OPENAI_API_KEY"

	openAIBatch    = 100
	openAIAttempts = 4
)

type openAI struct {
	endpoint, model string
	client          *http.Client
	backoff         func(attempt int) time.Duration
}

func newOpenAI(s Spec) *openAI {
	o := &openAI{
		endpoint: strings.TrimRight(s.Endpoint, "/"),
		model:    s.Model,
		client:   &http.Client{Timeout: 2 * time.Minute},
		backoff:  func(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second },
	}
	if o.endpoint == "" {
		o.endpoint = DefaultOpenAIEndpoint
	}
	if o.model == "" {
		o.model = DefaultOpenAIModel
	}
	return o
}

func (o *openAI) Key() string {
	return "openai__" + strings.NewReplacer(":", "_", "/", "_").Replace(o.model) + "__l2__" + recipe
}

// Embed sends the batches one after another: the API's limits are per minute,
// and batches in flight together only reach them sooner.
func (o *openAI) Embed(ctx context.Context, texts []string) ([][]float32, usage.Tokens, error) {
	var total usage.Tokens
	key := os.Getenv(openAIKeyEnv)
	if key == "" {
		return nil, total, fmt.Errorf("the openai embedder reads its key from %s, which is not set", openAIKeyEnv)
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += openAIBatch {
		vecs, tokens, err := o.embedBatch(ctx, key, texts[start:min(start+openAIBatch, len(texts))])
		if err != nil {
			// What the batches before this one took is still owed.
			return nil, total, err
		}
		total.Add(tokens)
		out = append(out, vecs...)
	}
	return out, total, nil
}

func (o *openAI) EmbedQuery(ctx context.Context, q string) ([]float32, usage.Tokens, error) {
	vecs, tokens, err := o.Embed(ctx, []string{q})
	if err != nil {
		return nil, tokens, err
	}
	return vecs[0], tokens, nil
}

// retryable is a 429 or a 5xx: the same request may succeed later.
type retryable struct{ error }

func (o *openAI) embedBatch(ctx context.Context, key string, texts []string) ([][]float32, usage.Tokens, error) {
	var err error
	for attempt := range openAIAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, usage.Tokens{}, ctx.Err()
			case <-time.After(o.backoff(attempt)):
			}
		}
		var vecs [][]float32
		var tokens usage.Tokens
		if vecs, tokens, err = o.post(ctx, key, texts); err == nil {
			return vecs, tokens, nil
		}
		if !errors.As(err, &retryable{}) || ctx.Err() != nil {
			return nil, usage.Tokens{}, err
		}
	}
	return nil, usage.Tokens{}, err
}

func (o *openAI) post(ctx context.Context, key string, texts []string) ([][]float32, usage.Tokens, error) {
	body, err := json.Marshal(map[string]any{"model": o.model, "input": texts, "encoding_format": "float"})
	if err != nil {
		return nil, usage.Tokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, usage.Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, usage.Tokens{}, retryable{fmt.Errorf("openai at %s: %w", o.endpoint, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("openai %s: status %d: %s", o.model, resp.StatusCode, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, usage.Tokens{}, retryable{err}
		}
		return nil, usage.Tokens{}, err
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			Prompt int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, usage.Tokens{}, fmt.Errorf("openai response: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, usage.Tokens{}, fmt.Errorf("openai returned %d embeddings for %d inputs", len(out.Data), len(texts))
	}
	// The API numbers its answers; the order is not promised.
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) || vecs[d.Index] != nil || len(d.Embedding) == 0 {
			return nil, usage.Tokens{}, fmt.Errorf("openai response: embedding index %d of %d is out of range, repeated or empty", d.Index, len(vecs))
		}
		Normalize(d.Embedding)
		vecs[d.Index] = d.Embedding
	}
	return vecs, usage.Tokens{In: out.Usage.Prompt, Requests: 1}, nil
}
