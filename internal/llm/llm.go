// Package llm is the chat client of the semantic layer's model extractor: one
// call that takes a system prompt, a user prompt and the JSON schema of the
// answer, over four providers. Plain net/http, no SDKs.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kgatilin/muninn/internal/usage"
)

// Client asks a chat model one question and returns its answer, which is JSON
// text meant to follow schema. The providers constrain the output where they
// can; the caller still parses and validates what comes back.
type Client interface {
	// The tokens are what the provider says the call took; a local model
	// reports none.
	Complete(ctx context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error)
	// ID is provider:model, as the extractor's id carries it.
	ID() string
}

// Spec is a chat model as a bank states it. Endpoint overrides the provider's
// base URL.
type Spec struct {
	Provider, Model, Endpoint string
}

type provider struct {
	model  string // the default: small and cheap
	keyEnv string
	make   func(s Spec, key string) Client
}

var providers = map[string]provider{
	"ollama":    {model: "qwen3:4b", make: func(s Spec, _ string) Client { return newOllama(s) }},
	"gemini":    {model: "gemini-2.5-flash-lite", keyEnv: "GEMINI_API_KEY", make: func(s Spec, k string) Client { return newGemini(s, k) }},
	"vertex":    {model: "gemini-3.5-flash-lite", make: func(s Spec, _ string) Client { return newVertex(s) }},
	"anthropic": {model: "claude-haiku-4-5", keyEnv: "ANTHROPIC_API_KEY", make: func(s Spec, k string) Client { return newAnthropic(s, k) }},
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
// colons, as Ollama tags do. A provider alone takes its default model.
func ParseSpec(v string) (Spec, error) {
	name, model, _ := strings.Cut(v, ":")
	p, ok := providers[name]
	if !ok {
		return Spec{}, fmt.Errorf("unknown chat provider %q; accepted: %s", name, strings.Join(Providers(), ", "))
	}
	if model == "" {
		model = p.model
	}
	return Spec{Provider: name, Model: model}, nil
}

func (s Spec) String() string { return s.Provider + ":" + s.Model }

// New is the client for a spec. A hosted provider's key is read here, from the
// environment of the run, and kept nowhere else; a missing key is an error
// before any request is made.
func New(s Spec) (Client, error) {
	p, ok := providers[s.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown chat provider %q; accepted: %s", s.Provider, strings.Join(Providers(), ", "))
	}
	if s.Model == "" {
		s.Model = p.model
	}
	var key string
	if p.keyEnv != "" {
		if key = os.Getenv(p.keyEnv); key == "" {
			return nil, fmt.Errorf("%s: %s is not set in the environment of this run", s.Provider, p.keyEnv)
		}
	}
	return p.make(s, key), nil
}

// ErrNoAnswer is a request the provider served and the model did not answer:
// a refusal, a blocked prompt, an answer cut at the token limit. It is one
// passage's problem and not the run's.
var ErrNoAnswer = errors.New("the model gave no answer")

const attempts = 4

// backoff is the wait before the second attempt; it doubles from there. A
// variable so that tests do not sleep.
var backoff = time.Second

var httpClient = &http.Client{Timeout: 10 * time.Minute}

// statusError is a response that was not 200.
type statusError struct {
	provider string
	code     int
	body     string
	after    time.Duration
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.provider, e.code, e.body)
}

func (e *statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code == 529 || e.code >= 500
}

// postJSON sends body and decodes a 200 into out. A 429, a 5xx and a network
// error are tried again, a few times, waiting what Retry-After says or a
// doubling backoff; a cancelled context stops the wait.
func postJSON(ctx context.Context, name, url string, header map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	wait := backoff
	for attempt := 1; ; attempt++ {
		err = postOnce(ctx, name, url, header, payload, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		se, isStatus := err.(*statusError)
		if attempt == attempts || (isStatus && !se.retryable()) {
			return err
		}
		pause := wait
		if isStatus && se.after > 0 {
			pause = se.after
		}
		wait *= 2
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}
}

func postOnce(ctx context.Context, name, url string, header map[string]string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", name, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		se := &statusError{provider: name, code: resp.StatusCode, body: strings.TrimSpace(string(msg))}
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			se.after = min(time.Duration(secs)*time.Second, time.Minute)
		}
		return se
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s response: %w", name, err)
	}
	return nil
}
