// Package llm is a minimal OpenAI chat/completions client used by the semantic
// extraction stage. It requests JSON mode and retries once on transient errors.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retryDelay is the default backoff between attempts when no Retry-After is
// present; overridable in tests.
var retryDelay = 500 * time.Millisecond

// jitterFrac controls the maximum random jitter added to the fixed backoff as
// a fraction of retryDelay (0 disables jitter; overridable in tests).
var jitterFrac int64 = 2 // jitter up to retryDelay/2

// maxRetryDelay caps the Retry-After value so one bad gateway cannot stall the
// pipeline indefinitely.
const maxRetryDelay = 60 * time.Second

// parseRetryAfter returns the delay from a Retry-After header value. It
// handles both the integer-seconds form ("30") and the HTTP-date form
// ("Mon, 02 Jan 2006 15:04:05 GMT"). Returns 0 when the header is absent or
// unparseable.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	// Try integer seconds first.
	if secs, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil {
		d := time.Duration(secs * float64(time.Second))
		if d < 0 {
			return 0
		}
		return d
	}
	// Try HTTP-date.
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// Config holds the OpenAI client configuration.
type Config struct {
	APIKey  string
	Model   string
	BaseURL string
}

// ConfigFromEnv reads OPENAI_API_KEY, SEMANTIC_MODEL (default gpt-4o-mini), and
// OPENAI_BASE_URL (default https://api.openai.com/v1, trailing slash trimmed).
func ConfigFromEnv(getenv func(string) string) Config {
	model := getenv("SEMANTIC_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	base := getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return Config{
		APIKey:  getenv("OPENAI_API_KEY"),
		Model:   model,
		BaseURL: strings.TrimRight(base, "/"),
	}
}

// Client posts chat/completions to an OpenAI-compatible endpoint.
type Client struct {
	cfg  Config
	http *http.Client
}

// New constructs an OpenAI client.
func New(cfg Config, hc *http.Client) *Client {
	return &Client{cfg: cfg, http: hc}
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	ResponseFormat map[string]string `json:"response_format"`
	Temperature    float64           `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete sends a single user prompt in JSON mode and returns the message
// content. It retries once on 429/5xx, honouring Retry-After when present.
func (c *Client) Complete(ctx context.Context, prompt string) (string, error) {
	reqBody := chatRequest{
		Model:          c.cfg.Model,
		Messages:       []chatMessage{{Role: "user", Content: prompt}},
		ResponseFormat: map[string]string{"type": "json_object"},
		Temperature:    0,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	var lastErr error
	nextDelay := retryDelay
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(nextDelay):
			}
		}
		content, retry, wait, err := c.try(ctx, b)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retry {
			return "", err
		}
		// Use server-supplied Retry-After when present, capped to maxRetryDelay.
		if wait > 0 {
			if wait > maxRetryDelay {
				wait = maxRetryDelay
			}
			nextDelay = wait
		} else if jitterFrac > 0 {
			// No Retry-After: spread concurrent retries with random jitter so
			// all goroutines in the errgroup do not re-fire simultaneously.
			nextDelay = retryDelay + time.Duration(rand.Int64N(int64(retryDelay)/jitterFrac)) //nolint:gosec
		}
	}
	return "", lastErr
}

// try performs one request. retry is true only for transient (429/5xx)
// failures. wait is the Retry-After duration parsed from the response (0 when
// absent or on success).
func (c *Client) try(ctx context.Context, body []byte) (content string, retry bool, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", false, 0, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", true, 0, fmt.Errorf("call openai: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_, _ = io.Copy(io.Discard, resp.Body)
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		wait = parseRetryAfter(resp.Header.Get("Retry-After"))
		return "", transient, wait, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(b))
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", false, 0, fmt.Errorf("decode openai response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", false, 0, fmt.Errorf("openai response has no choices")
	}
	return cr.Choices[0].Message.Content, false, 0, nil
}
