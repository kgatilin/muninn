package embed

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/gcp/gcptest"
	"github.com/kgatilin/muninn/internal/usage"
)

// vertexServer answers predict with one vector per instance, [3, 0, 4·len].
// refuse is asked first with the size of the batch and may answer in its place.
func vertexServer(t *testing.T, tasks *[]string, sizes *[]int, refuse func(n int, w http.ResponseWriter) bool) *vertex {
	t.Helper()
	gcptest.Setup(t)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v1/projects/" + gcptest.Project + "/locations/global/publishers/google/models/" + DefaultVertexModel + ":predict"
		if r.URL.Path != want || r.Header.Get("Authorization") != "Bearer "+gcptest.Token {
			t.Errorf("%s %v", r.URL.Path, r.Header)
		}
		var req struct {
			Instances []struct {
				Content  string `json:"content"`
				TaskType string `json:"task_type"`
			} `json:"instances"`
			Parameters struct {
				Dims int `json:"outputDimensionality"`
			} `json:"parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Parameters.Dims != vertexDims {
			t.Errorf("request: %v %+v", err, req.Parameters)
		}
		mu.Lock()
		defer mu.Unlock()
		if refuse != nil && refuse(len(req.Instances), w) {
			return
		}
		*sizes = append(*sizes, len(req.Instances))
		type prediction struct {
			Embeddings struct {
				Values     []float32      `json:"values"`
				Statistics map[string]int `json:"statistics"`
			} `json:"embeddings"`
		}
		preds := make([]prediction, len(req.Instances))
		for i, in := range req.Instances {
			*tasks = append(*tasks, in.TaskType)
			preds[i].Embeddings.Statistics = map[string]int{"token_count": 3}
			preds[i].Embeddings.Values = []float32{3, 0, 4 * float32(len(in.Content))}
		}
		json.NewEncoder(w).Encode(map[string]any{"predictions": preds})
	}))
	t.Cleanup(srv.Close)
	v := newVertex(Spec{Provider: "vertex", Endpoint: srv.URL + "/"})
	v.backoff = func(int) time.Duration { return time.Millisecond }
	return v
}

func TestVertexBatchesAndNormalizes(t *testing.T) {
	var tasks []string
	var sizes []int
	v := vertexServer(t, &tasks, &sizes, nil)
	texts := make([]string, vertexBatch+3)
	for i := range texts {
		texts[i] = "a"
	}
	texts[vertexBatch] = "bb"
	vecs, tokens, err := v.Embed(context.Background(), texts)
	if err != nil || len(vecs) != len(texts) || len(sizes) != 2 || tokens != (usage.Tokens{In: 3 * len(texts), Requests: 2}) {
		t.Fatalf("%d vectors in %v requests, %v: %v", len(vecs), sizes, tokens, err)
	}
	if got := vecs[vertexBatch]; math.Abs(float64(got[2])-8.0/math.Sqrt(73)) > 1e-6 {
		t.Fatalf("the vector of the longer text: %v", got)
	}
	if _, _, err := v.EmbedQuery(context.Background(), "q"); err != nil || tasks[0] != "RETRIEVAL_DOCUMENT" || tasks[len(tasks)-1] != "RETRIEVAL_QUERY" {
		t.Fatalf("%v %v", err, tasks)
	}
}

func TestVertexHalvesARefusedBatchAndRetries(t *testing.T) {
	var tasks []string
	var sizes []int
	limited := false
	v := vertexServer(t, &tasks, &sizes, func(n int, w http.ResponseWriter) bool {
		switch {
		case n > 2:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "too many tokens")
			return true
		case !limited:
			limited = true
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	})
	vecs, tokens, err := v.Embed(context.Background(), []string{"a", "bb", "ccc", "dddd", "eeeee"})
	// Only the served requests are counted: 2+1+2 texts after the halving.
	if err != nil || len(vecs) != 5 || tokens != (usage.Tokens{In: 15, Requests: 3}) {
		t.Fatalf("%v %v %v", vecs, tokens, err)
	}
	for i, vec := range vecs {
		n := 4 * float64(i+1)
		if math.Abs(float64(vec[2])-n/math.Sqrt(9+n*n)) > 1e-6 {
			t.Fatalf("vector %d out of place: %v", i, vec)
		}
	}
}
