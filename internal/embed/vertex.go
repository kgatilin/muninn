package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kgatilin/muninn/internal/gcp"
	"github.com/kgatilin/muninn/internal/usage"
)

// Vertex AI's text embeddings: the `predict` method of a Google publisher
// model, many texts to a request, several requests in flight. The project and
// location come from the environment of the run and the bearer token from the
// application default credentials, both when the embedder is first used, so a
// bank on this provider can be made, shown and searched lexically without them.
//
// The model prompts a document and a query differently through task_type.

const (
	DefaultVertexModel = "gemini-embedding-001"

	// vertexDims is asked of the model, which is trained to be cut short:
	// a quarter of its full width, at little cost in retrieval.
	vertexDims = 768
	// vertexBatch is well under the API's 250 instances: its other limit is
	// the tokens of one request, which a batch of long chunks reaches first.
	vertexBatch       = 50
	vertexConcurrency = 4
	vertexAttempts    = 6
)

type vertex struct {
	endpoint, model string
	client          *http.Client
	backoff         func(attempt int) time.Duration
}

func newVertex(s Spec) *vertex {
	v := &vertex{
		endpoint: s.Endpoint,
		model:    s.Model,
		client:   &http.Client{Timeout: 2 * time.Minute},
		backoff:  func(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second },
	}
	if v.model == "" {
		v.model = DefaultVertexModel
	}
	return v
}

func (v *vertex) Key() string {
	return fmt.Sprintf("vertex__%s__%d__l2__%s", strings.NewReplacer(":", "_", "/", "_").Replace(v.model), vertexDims, recipe)
}

func (v *vertex) Embed(ctx context.Context, texts []string) ([][]float32, usage.Tokens, error) {
	return v.embedAll(ctx, texts, "RETRIEVAL_DOCUMENT")
}

func (v *vertex) EmbedQuery(ctx context.Context, q string) ([]float32, usage.Tokens, error) {
	vecs, tokens, err := v.embedAll(ctx, []string{q}, "RETRIEVAL_QUERY")
	if err != nil {
		return nil, tokens, err
	}
	return vecs[0], tokens, nil
}

// embedAll returns the tokens of the batches that were served even when
// another batch failed the call: they are still owed.
func (v *vertex) embedAll(ctx context.Context, texts []string, task string) ([][]float32, usage.Tokens, error) {
	var total usage.Tokens
	url, err := gcp.ModelURL(v.endpoint, v.model, "predict")
	if err != nil {
		return nil, total, fmt.Errorf("the vertex embedder: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([][]float32, len(texts))
	sem := make(chan struct{}, vertexConcurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		once     sync.Once
		firstErr error
	)
	for start := 0; start < len(texts) && ctx.Err() == nil; start += vertexBatch {
		end := min(start+vertexBatch, len(texts))
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			vecs, tokens, err := v.embedBatch(ctx, url, texts[start:end], task)
			mu.Lock()
			total.Add(tokens)
			mu.Unlock()
			if err != nil {
				once.Do(func() { firstErr = err; cancel() })
				return
			}
			copy(results[start:end], vecs)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, total, firstErr
	}
	return results, total, ctx.Err()
}

// tooLarge is a 400: the request as a whole was refused, which for texts the
// model truncates by itself means the batch held too many tokens.
type tooLarge struct{ error }

// embedBatch retries a 429 and a 5xx, and halves a batch the API refuses.
func (v *vertex) embedBatch(ctx context.Context, url string, texts []string, task string) ([][]float32, usage.Tokens, error) {
	var err error
	for attempt := range vertexAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, usage.Tokens{}, ctx.Err()
			case <-time.After(v.backoff(attempt)):
			}
		}
		var vecs [][]float32
		var tokens usage.Tokens
		if vecs, tokens, err = v.post(ctx, url, texts, task); err == nil {
			return vecs, tokens, nil
		}
		if ctx.Err() != nil {
			return nil, usage.Tokens{}, ctx.Err()
		}
		if errors.As(err, &tooLarge{}) && len(texts) > 1 {
			half := len(texts) / 2
			left, total, err := v.embedBatch(ctx, url, texts[:half], task)
			if err != nil {
				return nil, total, err
			}
			right, tokens, err := v.embedBatch(ctx, url, texts[half:], task)
			total.Add(tokens)
			if err != nil {
				return nil, total, err
			}
			return append(left, right...), total, nil
		}
		if !errors.As(err, &retryable{}) {
			return nil, usage.Tokens{}, err
		}
	}
	return nil, usage.Tokens{}, err
}

func (v *vertex) post(ctx context.Context, url string, texts []string, task string) ([][]float32, usage.Tokens, error) {
	token, err := gcp.Token(ctx)
	if err != nil {
		return nil, usage.Tokens{}, fmt.Errorf("the vertex embedder: %w", err)
	}
	instances := make([]map[string]string, len(texts))
	for i, t := range texts {
		instances[i] = map[string]string{"content": t, "task_type": task}
	}
	body, err := json.Marshal(map[string]any{
		"instances":  instances,
		"parameters": map[string]any{"outputDimensionality": vertexDims, "autoTruncate": true},
	})
	if err != nil {
		return nil, usage.Tokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, usage.Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, usage.Tokens{}, retryable{fmt.Errorf("vertex at %s: %w", url, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("vertex %s: status %d: %s", v.model, resp.StatusCode, strings.TrimSpace(string(msg)))
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return nil, usage.Tokens{}, retryable{err}
		case resp.StatusCode == http.StatusBadRequest:
			return nil, usage.Tokens{}, tooLarge{err}
		}
		return nil, usage.Tokens{}, err
	}
	var out struct {
		Predictions []struct {
			Embeddings struct {
				Values     []float32 `json:"values"`
				Statistics struct {
					Tokens int `json:"token_count"`
				} `json:"statistics"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, usage.Tokens{}, fmt.Errorf("vertex response: %w", err)
	}
	if len(out.Predictions) != len(texts) {
		return nil, usage.Tokens{}, fmt.Errorf("vertex returned %d embeddings for %d inputs", len(out.Predictions), len(texts))
	}
	vecs := make([][]float32, len(texts))
	tokens := usage.Tokens{Requests: 1}
	for i, p := range out.Predictions {
		if len(p.Embeddings.Values) == 0 {
			return nil, usage.Tokens{}, fmt.Errorf("vertex response: embedding %d of %d is empty", i, len(vecs))
		}
		// A shortened embedding does not come back at unit length.
		Normalize(p.Embeddings.Values)
		tokens.In += p.Embeddings.Statistics.Tokens
		vecs[i] = p.Embeddings.Values
	}
	return vecs, tokens, nil
}
