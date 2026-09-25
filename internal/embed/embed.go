// Package embed holds the embedders. The interface is asymmetric because the
// local models worth using prompt a document and a query differently.
package embed

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"strings"

	"github.com/kgatilin/muninn/internal/usage"
)

type Embedder interface {
	// The tokens are what the provider says the call took, for the usage
	// log; a local model reports none.
	Embed(ctx context.Context, texts []string) ([][]float32, usage.Tokens, error)
	EmbedQuery(ctx context.Context, query string) ([]float32, usage.Tokens, error)
	// Key names the vector space: provider, model, normalization and recipe.
	// Vectors under different keys are never compared.
	Key() string
}

// Spec is an embedder as a bank states it.
type Spec struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

func (s Spec) String() string {
	if s.Model == "" {
		return s.Provider
	}
	return s.Provider + ":" + s.Model
}

// recipe changes when the text handed to a model for the same chunk changes.
const recipe = "r1"

var providers = map[string]func(Spec) Embedder{
	"ollama": func(s Spec) Embedder { return newOllama(s) },
	"openai": func(s Spec) Embedder { return newOpenAI(s) },
	"vertex": func(s Spec) Embedder { return newVertex(s) },
	"noop":   func(Spec) Embedder { return noop{} },
}

func Providers() []string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseSpec reads "provider" or "provider:model"; a model may itself contain
// colons, as Ollama tags do.
func ParseSpec(v string) (Spec, error) {
	provider, model, _ := strings.Cut(v, ":")
	if _, ok := providers[provider]; !ok {
		return Spec{}, fmt.Errorf("unknown embedder provider %q; accepted: %s", provider, strings.Join(Providers(), ", "))
	}
	if model == "" {
		model = map[string]string{"ollama": DefaultOllamaModel, "openai": DefaultOpenAIModel, "vertex": DefaultVertexModel}[provider]
	}
	return Spec{Provider: provider, Model: model}, nil
}

func New(s Spec) (Embedder, error) {
	mk, ok := providers[s.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown embedder provider %q; accepted: %s", s.Provider, strings.Join(Providers(), ", "))
	}
	return mk(s), nil
}

// Normalize scales v to unit length in place, so cosine is a dot product.
func Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// noop is deterministic pseudo-vectors: tests, and a bank with no model at hand.
type noop struct{}

func (noop) Key() string { return "noop__l2__" + recipe }

func (noop) Embed(_ context.Context, texts []string) ([][]float32, usage.Tokens, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = pseudo(t)
	}
	return out, usage.Tokens{}, nil
}

func (noop) EmbedQuery(_ context.Context, q string) ([]float32, usage.Tokens, error) {
	return pseudo(q), usage.Tokens{}, nil
}

func pseudo(s string) []float32 {
	h := fnv.New64a()
	h.Write([]byte(s))
	r := rand.New(rand.NewSource(int64(h.Sum64())))
	v := make([]float32, 64)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	Normalize(v)
	return v
}
