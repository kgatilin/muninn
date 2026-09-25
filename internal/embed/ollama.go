package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kgatilin/muninn/internal/usage"
)

// Lifted from wyrd's adapter/embed/ollama: batched /api/embed, several batches
// in flight, model-specific document and query prompts.

const (
	DefaultOllamaEndpoint = "http://localhost:11434"
	DefaultOllamaModel    = "qwen3-embedding:0.6b"

	ollamaBatch       = 32
	ollamaConcurrency = 4
	ollamaAttempts    = 3

	queryInstruction = "Given a search query, retrieve relevant passages that answer it."
)

type promptStyle int

const (
	styleRaw promptStyle = iota
	styleQwen3
	styleGemma
	styleNomic
)

func detectStyle(model string) promptStyle {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "qwen3-embedding"):
		return styleQwen3
	case strings.Contains(m, "embeddinggemma"):
		return styleGemma
	case strings.Contains(m, "nomic-embed"):
		return styleNomic
	}
	return styleRaw
}

type ollama struct {
	endpoint, model string
	style           promptStyle
	client          *http.Client
}

func newOllama(s Spec) *ollama {
	o := &ollama{endpoint: s.Endpoint, model: s.Model, client: &http.Client{Timeout: 10 * time.Minute}}
	if o.endpoint == "" {
		o.endpoint = DefaultOllamaEndpoint
	}
	if o.model == "" {
		o.model = DefaultOllamaModel
	}
	o.style = detectStyle(o.model)
	return o
}

func (o *ollama) Key() string {
	return "ollama__" + strings.NewReplacer(":", "_", "/", "_").Replace(o.model) + "__l2__" + recipe
}

func (o *ollama) docPrompt(t string) string {
	switch o.style {
	case styleGemma:
		return "title: none | text: " + t
	case styleNomic:
		return "search_document: " + t
	}
	return t
}

func (o *ollama) queryPrompt(q string) string {
	switch o.style {
	case styleQwen3:
		return "Instruct: " + queryInstruction + "\nQuery: " + q
	case styleGemma:
		return "task: search result | query: " + q
	case styleNomic:
		return "search_query: " + q
	}
	return q
}

func (o *ollama) Embed(ctx context.Context, texts []string) ([][]float32, usage.Tokens, error) {
	prompts := make([]string, len(texts))
	for i, t := range texts {
		prompts[i] = o.docPrompt(t)
	}
	vecs, err := o.embedAll(ctx, prompts)
	return vecs, usage.Tokens{}, err
}

func (o *ollama) EmbedQuery(ctx context.Context, q string) ([]float32, usage.Tokens, error) {
	vecs, err := o.embedAll(ctx, []string{o.queryPrompt(q)})
	if err != nil {
		return nil, usage.Tokens{}, err
	}
	return vecs[0], usage.Tokens{}, nil
}

func (o *ollama) embedAll(ctx context.Context, prompts []string) ([][]float32, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([][]float32, len(prompts))
	sem := make(chan struct{}, ollamaConcurrency)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for start := 0; start < len(prompts) && ctx.Err() == nil; start += ollamaBatch {
		end := min(start+ollamaBatch, len(prompts))
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			vecs, err := o.embedBatch(ctx, prompts[start:end])
			if err != nil {
				once.Do(func() { firstErr = err; cancel() })
				return
			}
			copy(results[start:end], vecs)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, ctx.Err()
}

func (o *ollama) embedBatch(ctx context.Context, prompts []string) ([][]float32, error) {
	var err error
	for attempt := range ollamaAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		var vecs [][]float32
		if vecs, err = o.post(ctx, prompts); err == nil {
			return vecs, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}

func (o *ollama) post(ctx context.Context, prompts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": o.model, "input": prompts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama at %s: %w", o.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama %s: status %d: %s", o.model, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama response: %w", err)
	}
	if len(out.Embeddings) != len(prompts) {
		return nil, fmt.Errorf("ollama returned %d embeddings for %d inputs", len(out.Embeddings), len(prompts))
	}
	for _, v := range out.Embeddings {
		Normalize(v)
	}
	return out.Embeddings, nil
}
