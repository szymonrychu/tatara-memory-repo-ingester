package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
