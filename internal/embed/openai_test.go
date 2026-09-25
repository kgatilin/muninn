package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/usage"
)

// openAIServer answers /v1/embeddings with one vector per input, [i+1, 0, 0]
// scaled by 3 so normalization shows, in reverse order so the index is what
// places them. fail is asked first and may answer in its place.
func openAIServer(t *testing.T, sizes *[]int, fail func(w http.ResponseWriter) bool) *openAI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization %q", got)
		}
		var req struct {
			Model          string   `json:"model"`
			Input          []string `json:"input"`
			EncodingFormat string   `json:"encoding_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "text-embedding-3-small" || req.EncodingFormat != "float" {
			t.Errorf("request model %q, encoding_format %q", req.Model, req.EncodingFormat)
		}
		if fail != nil && fail(w) {
			return
		}
		*sizes = append(*sizes, len(req.Input))
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := len(req.Input) - 1; i >= 0; i-- {
			data = append(data, item{Index: i, Embedding: []float32{3, 0, 4 * float32(len(req.Input[i]))}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"prompt_tokens": 2 * len(req.Input)}})
	}))
	t.Cleanup(srv.Close)
	o := newOpenAI(Spec{Provider: "openai", Endpoint: srv.URL + "/"})
	o.backoff = func(int) time.Duration { return time.Millisecond }
	return o
}

func TestOpenAIBatchesAndNormalizes(t *testing.T) {
	t.Setenv(openAIKeyEnv, "sk-test")
	var sizes []int
	o := openAIServer(t, &sizes, nil)
	texts := make([]string, 250)
	for i := range texts {
		texts[i] = strings.Repeat("x", i%3) // lengths 0, 1, 2 tell the vectors apart
	}
	vecs, tokens, err := o.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 100 || sizes[2] != 50 {
		t.Errorf("batches %v, want 100 100 50", sizes)
	}
	if tokens != (usage.Tokens{In: 500, Requests: 3}) {
		t.Errorf("tokens %v", tokens)
	}
	for i, v := range vecs {
		z := 4 * float64(i%3)
		norm := math.Sqrt(9 + z*z)
		if len(v) != 3 || math.Abs(float64(v[0])-3/norm) > 1e-6 || math.Abs(float64(v[2])-z/norm) > 1e-6 {
			t.Fatalf("vector %d is %v: not the one for its input, or not unit length", i, v)
		}
	}
	if o.Key() != "openai__text-embedding-3-small__l2__r1" {
		t.Errorf("key %q", o.Key())
	}
}

func TestOpenAIRetriesWhatMaySucceedLater(t *testing.T) {
	t.Setenv(openAIKeyEnv, "sk-test")
	var (
		sizes []int
		calls atomic.Int32
	)
	o := openAIServer(t, &sizes, func(w http.ResponseWriter) bool {
		switch calls.Add(1) {
		case 1:
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case 2:
			http.Error(w, "oops", http.StatusBadGateway)
		default:
			return false
		}
		return true
	})
	if _, _, err := o.EmbedQuery(context.Background(), "q"); err != nil {
		t.Fatalf("a 429 and a 502 before a 200: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("%d calls, want 3", calls.Load())
	}
}

func TestOpenAIGivesUp(t *testing.T) {
	t.Setenv(openAIKeyEnv, "sk-test")
	var (
		sizes []int
		calls atomic.Int32
		code  = http.StatusServiceUnavailable
	)
	o := openAIServer(t, &sizes, func(w http.ResponseWriter) bool {
		calls.Add(1)
		http.Error(w, "no", code)
		return true
	})
	if _, _, err := o.EmbedQuery(context.Background(), "q"); err == nil || calls.Load() != openAIAttempts {
		t.Errorf("a 503 every time: err %v after %d calls, want %d", err, calls.Load(), openAIAttempts)
	}
	calls.Store(0)
	code = http.StatusUnauthorized
	if _, _, err := o.EmbedQuery(context.Background(), "q"); err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "401") {
		t.Errorf("a 401 is not retried: err %v after %d calls", err, calls.Load())
	}
}

func TestOpenAIMissingKeyIsSaidAtFirstUse(t *testing.T) {
	t.Setenv(openAIKeyEnv, "")
	spec, err := ParseSpec("openai")
	if err != nil || spec.Model != DefaultOpenAIModel {
		t.Fatalf("spec %+v, err %v", spec, err)
	}
	e, err := New(spec)
	if err != nil {
		t.Fatalf("making the embedder needs no key: %v", err)
	}
	if _, _, err := e.Embed(context.Background(), []string{"a"}); err == nil || !strings.Contains(err.Error(), openAIKeyEnv) {
		t.Errorf("err %v does not name %s", err, openAIKeyEnv)
	}
}
