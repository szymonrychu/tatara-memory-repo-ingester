package obs_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/obs"
)

// TestNewRegistersAllMetrics verifies New() returns a non-nil Metrics with all
// fields populated (i.e., nothing panicked during registration).
func TestNewRegistersAllMetrics(t *testing.T) {
	m := obs.New()
	require.NotNil(t, m)
	require.NotNil(t, m.IngestRunsTotal)
	require.NotNil(t, m.PushRequestsTotal)
	require.NotNil(t, m.IngestStageDuration)
	require.NotNil(t, m.SemanticMissesTotal)
	require.NotNil(t, m.LLMCallsTotal)
	require.NotNil(t, m.AnalyzerEntitiesTotal)
	require.NotNil(t, m.AnalyzerEdgesTotal)
	require.NotNil(t, m.AnalyzerParseErrorsTotal)
	require.NotNil(t, m.AnalyzerDuration)
	require.NotNil(t, m.IngestFilesQuarantinedTotal)
	require.NotNil(t, m.SCIPEntitiesTotal)
	require.NotNil(t, m.SCIPEdgesTotal)
	require.NotNil(t, m.SemanticChunkExtractionsTotal)
}

// TestPushSendsTextMetrics verifies that Push serialises incremented counters
// to the receiver as Prometheus text format.
func TestPushSendsTextMetrics(t *testing.T) {
	m := obs.New()
	m.IngestRunsTotal.Inc()
	m.SCIPEntitiesTotal.Add(3)
	m.AnalyzerEntitiesTotal.WithLabelValues("go").Add(5)
	m.SemanticChunkExtractionsTotal.WithLabelValues("ok").Inc()
	m.SemanticChunkExtractionsTotal.WithLabelValues("fail").Add(2)

	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := m.Push(context.Background(), srv.URL, http.DefaultClient)
	require.NoError(t, err)

	assert.Contains(t, received, "ingest_runs_total", "text output must contain ingest_runs_total")
	assert.Contains(t, received, "scip_entities_total", "text output must contain scip_entities_total")
	assert.Contains(t, received, `language="go"`, "label must be present in output")
	assert.Contains(t, received, `result="fail"`, "fail label must be present")
}

// TestNewRegistersIngestRunResultTotal verifies the result-labeled run counter
// is present. This is a regression test for the missing result dimension on
// IngestRunsTotal (finding 1: no success/failure outcome metric).
func TestNewRegistersIngestRunResultTotal(t *testing.T) {
	m := obs.New()
	require.NotNil(t, m.IngestRunResultTotal, "IngestRunResultTotal must be registered")
	// Incrementing both labels must not panic.
	m.IngestRunResultTotal.WithLabelValues("success").Inc()
	m.IngestRunResultTotal.WithLabelValues("failure").Inc()
}

// TestPushCarriesRunResultTotal verifies that result-labeled counts appear in
// the pushed text, so the operator can alert on ingest failures.
func TestPushCarriesRunResultTotal(t *testing.T) {
	m := obs.New()
	m.IngestRunResultTotal.WithLabelValues("failure").Inc()

	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, m.Push(context.Background(), srv.URL, http.DefaultClient))
	assert.Contains(t, received, "ingest_run_result_total", "pushed text must include result counter")
	assert.Contains(t, received, `result="failure"`, "failure label must be present")
}

// TestPushReturnsErrorOnNon2xx verifies Push surfaces server errors.
func TestPushReturnsErrorOnNon2xx(t *testing.T) {
	m := obs.New()
	m.IngestRunsTotal.Inc() // ensure something to push

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := m.Push(context.Background(), srv.URL, http.DefaultClient)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "500"), "error should mention status 500")
}
