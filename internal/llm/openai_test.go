package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompleteSendsJSONModeRequest(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "sk-test", Model: "gpt-4o-mini", BaseURL: srv.URL}, http.DefaultClient)
	out, err := c.Complete(context.Background(), "do the thing")
	require.NoError(t, err)
	require.Equal(t, `{"ok":true}`, out)
	require.Equal(t, "Bearer sk-test", gotAuth)
	require.Equal(t, "gpt-4o-mini", gotBody["model"])
	rf := gotBody["response_format"].(map[string]any)
	require.Equal(t, "json_object", rf["type"])
	msgs := gotBody["messages"].([]any)
	require.GreaterOrEqual(t, len(msgs), 1)
	last := msgs[len(msgs)-1].(map[string]any)
	require.Equal(t, "user", last["role"])
	require.Equal(t, "do the thing", last["content"])
}

func TestCompleteRetriesOnce5xxThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":1}"}}]}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "k", Model: "m", BaseURL: srv.URL}, http.DefaultClient)
	out, err := c.Complete(context.Background(), "x")
	require.NoError(t, err)
	require.Equal(t, `{"ok":1}`, out)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestCompleteRetriesOnce429ThenFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "k", Model: "m", BaseURL: srv.URL}, http.DefaultClient)
	_, err := c.Complete(context.Background(), "x")
	require.Error(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls), "one initial + one retry only")
}

func TestCompleteDoesNotRetryOn400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "k", Model: "m", BaseURL: srv.URL}, http.DefaultClient)
	_, err := c.Complete(context.Background(), "x")
	require.Error(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "4xx (non-429) must not retry")
}

func TestCompleteRespectsRetryAfterHeader(t *testing.T) {
	var calls int32
	var receivedDelay time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":1}"}}]}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "k", Model: "m", BaseURL: srv.URL}, http.DefaultClient)

	start := time.Now()
	out, err := c.Complete(context.Background(), "x")
	receivedDelay = time.Since(start)

	require.NoError(t, err)
	require.Equal(t, `{"ok":1}`, out)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
	// Retry-After: 2 seconds must be honoured; allow 200ms tolerance for slow CI
	require.GreaterOrEqual(t, receivedDelay, 2*time.Second, "retry delay must respect Retry-After header")
}

func TestCompleteRetryAfterHTTPDate(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// HTTP-date 2 seconds in the future
			future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
			w.Header().Set("Retry-After", future)
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":2}"}}]}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "k", Model: "m", BaseURL: srv.URL}, http.DefaultClient)

	start := time.Now()
	out, err := c.Complete(context.Background(), "x")
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, `{"ok":2}`, out)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
	// Allow generous lower bound: the server sets Retry-After 2s in the future
	// at request-receive time; by the time we measure elapsed includes round-
	// trips, so 1s is enough to prove we waited meaningfully vs the 500ms default.
	require.GreaterOrEqual(t, elapsed, 1*time.Second, "HTTP-date Retry-After must be honoured")
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg := ConfigFromEnv(func(k string) string {
		switch k {
		case "OPENAI_API_KEY":
			return "sk-xyz"
		default:
			return ""
		}
	})
	require.Equal(t, "sk-xyz", cfg.APIKey)
	require.Equal(t, "gpt-4o-mini", cfg.Model)
	require.Equal(t, "https://api.openai.com/v1", cfg.BaseURL)
}

func TestConfigFromEnvOverrides(t *testing.T) {
	cfg := ConfigFromEnv(func(k string) string {
		switch k {
		case "OPENAI_API_KEY":
			return "sk-1"
		case "SEMANTIC_MODEL":
			return "gpt-4o"
		case "OPENAI_BASE_URL":
			return "http://localhost:1234/v1/"
		default:
			return ""
		}
	})
	require.Equal(t, "gpt-4o", cfg.Model)
	require.Equal(t, "http://localhost:1234/v1", cfg.BaseURL, "trailing slash trimmed")
}
