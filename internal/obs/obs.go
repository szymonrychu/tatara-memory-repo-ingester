// Package obs registers all Prometheus metrics for tatara-memory-repo-ingester.
// Short-lived Jobs cannot be scraped, so callers collect metrics here and push
// the gathered text to the operator's pushmetrics receiver (or a Pushgateway)
// at job end via Push.
package obs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// Metrics holds all registered counters and histograms.
type Metrics struct {
	// Ingest-level counters.
	IngestRunsTotal      prometheus.Counter
	IngestRunResultTotal *prometheus.CounterVec   // labels: result (success|failure)
	PushRequestsTotal    *prometheus.CounterVec   // labels: path, result (ok|err)
	IngestStageDuration  *prometheus.HistogramVec // labels: stage
	SemanticMissesTotal  prometheus.Counter
	LLMCallsTotal        *prometheus.CounterVec // labels: result (ok|fail)

	// Bulk-push back-pressure counters. The code graph is pushed as bounded
	// batches (one server transaction each) instead of one whole-repo
	// transaction, so batch count, retries and server load-shed responses are
	// the signals that say whether the client is being throttled.
	CodeGraphBatchesTotal  *prometheus.CounterVec // labels: result (ok|err)
	CodeGraphBatchRows     prometheus.Histogram
	PushRetriesTotal       *prometheus.CounterVec // labels: path, reason (shed_429|shed_503|server_5xx|network)
	PushShedResponsesTotal *prometheus.CounterVec // labels: path, status (429|503)

	// Analyzer-level counters (labels: language).
	AnalyzerEntitiesTotal    *prometheus.CounterVec   // labels: language
	AnalyzerEdgesTotal       *prometheus.CounterVec   // labels: language
	AnalyzerParseErrorsTotal *prometheus.CounterVec   // labels: language
	AnalyzerDuration         *prometheus.HistogramVec // labels: language
	// IngestFilesQuarantinedTotal counts diff-set files held at their last-good
	// graph+chunk rows because their analyzer hard-errored (labels: language, the
	// analyzer name). Reuses the language label so it correlates with the other
	// analyzer_* series on dashboards and alerts (operator#68).
	IngestFilesQuarantinedTotal *prometheus.CounterVec
	// IngestFilesSoftFailedTotal counts diff-set files held at their last-good
	// graph+chunk rows because the analyzer could not read or parse that one file
	// but kept its batch (labels: language). Deliberately a sibling series rather
	// than a reason label on IngestFilesQuarantinedTotal: quarantine is a
	// zero-base-rate hard-error signal alerted at >0, soft failures have a nonzero
	// steady state (an unparseable file fails on every diff that touches it), and
	// folding them together would make the quarantine alert fire continuously.
	IngestFilesSoftFailedTotal *prometheus.CounterVec

	// SCIP counters.
	SCIPEntitiesTotal prometheus.Counter
	SCIPEdgesTotal    prometheus.Counter

	// Semantic chunk counters (labels: result=ok|fail).
	SemanticChunkExtractionsTotal *prometheus.CounterVec

	reg *prometheus.Registry
}

// New creates and registers all metrics on a fresh registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg}

	m.IngestRunsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ingest_runs_total",
		Help: "Total number of ingest runs started.",
	})
	m.IngestRunResultTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_run_result_total",
		Help: "Terminal result of each ingest run (success|failure).",
	}, []string{"result"})
	m.PushRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "push_requests_total",
		Help: "Total push HTTP requests by path and result (ok|err).",
	}, []string{"path", "result"})
	m.IngestStageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ingest_stage_duration_seconds",
		Help:    "Duration of each ingest stage in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"stage"})
	m.SemanticMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "semantic_misses_total",
		Help: "Total semantic cache misses returned by the server.",
	})
	m.LLMCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_calls_total",
		Help: "Total LLM completion calls by result (ok|fail).",
	}, []string{"result"})

	m.CodeGraphBatchesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "code_graph_batches_total",
		Help: "Total /code-graph:bulk batches sent by result (ok|err). One batch is one server transaction.",
	}, []string{"result"})
	m.CodeGraphBatchRows = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "code_graph_batch_rows",
		Help:    "Rows (entities+edges+symbols+hyperedges) per /code-graph:bulk batch.",
		Buckets: []float64{10, 50, 100, 250, 500, 1000, 2000, 5000, 10000},
	})
	m.PushRetriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "push_retries_total",
		Help: "Total push HTTP requests retried by path and reason (shed_429|shed_503|server_5xx|network).",
	}, []string{"path", "reason"})
	m.PushShedResponsesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "push_shed_responses_total",
		Help: "Total server load-shed responses received by path and status (429|503).",
	}, []string{"path", "status"})

	m.AnalyzerEntitiesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "analyzer_entities_total",
		Help: "Total entities emitted by each analyzer.",
	}, []string{"language"})
	m.AnalyzerEdgesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "analyzer_edges_total",
		Help: "Total edges emitted by each analyzer.",
	}, []string{"language"})
	m.AnalyzerParseErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "analyzer_parse_errors_total",
		Help: "Total parse errors encountered per language.",
	}, []string{"language"})
	m.AnalyzerDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "analyzer_duration_seconds",
		Help:    "Time spent in each language analyzer.",
		Buckets: prometheus.DefBuckets,
	}, []string{"language"})
	m.IngestFilesQuarantinedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_files_quarantined_total",
		Help: "Total diff-set files quarantined because their analyzer hard-errored (excluded from graph Files and chunk reconcile to preserve their last-good rows).",
	}, []string{"language"})
	m.IngestFilesSoftFailedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_files_soft_failed_total",
		Help: "Total diff-set files whose read or parse failed inside an otherwise healthy analyzer batch (excluded from graph Files and chunk reconcile to preserve their last-good rows).",
	}, []string{"language"})

	m.SCIPEntitiesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "scip_entities_total",
		Help: "Total entities emitted from SCIP ingest.",
	})
	m.SCIPEdgesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "scip_edges_total",
		Help: "Total edges emitted from SCIP ingest.",
	})
	m.SemanticChunkExtractionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "semantic_chunk_extractions_total",
		Help: "Total semantic chunk LLM extractions by result (ok|fail).",
	}, []string{"result"})

	reg.MustRegister(
		m.IngestRunsTotal,
		m.IngestRunResultTotal,
		m.PushRequestsTotal,
		m.IngestStageDuration,
		m.SemanticMissesTotal,
		m.LLMCallsTotal,
		m.CodeGraphBatchesTotal,
		m.CodeGraphBatchRows,
		m.PushRetriesTotal,
		m.PushShedResponsesTotal,
		m.AnalyzerEntitiesTotal,
		m.AnalyzerEdgesTotal,
		m.AnalyzerParseErrorsTotal,
		m.AnalyzerDuration,
		m.IngestFilesQuarantinedTotal,
		m.IngestFilesSoftFailedTotal,
		m.SCIPEntitiesTotal,
		m.SCIPEdgesTotal,
		m.SemanticChunkExtractionsTotal,
	)
	return m
}

// pushTimeout caps how long a metrics push may block at job teardown so a hung
// receiver cannot delay the Job pod's exit.
const pushTimeout = 10 * time.Second

// Push gathers all metrics and HTTP POSTs the text format to pushURL.
// pushURL is the operator's pushmetrics receiver, e.g.
// http://tatara-operator/pushmetrics/ingest/<runID>.
// Errors are returned so callers can log them; they must not fail the ingest.
func (m *Metrics) Push(ctx context.Context, pushURL string, hc *http.Client) error {
	gathered, err := m.reg.Gather()
	if err != nil {
		return fmt.Errorf("obs: gather: %w", err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range gathered {
		if err := enc.Encode(mf); err != nil {
			return fmt.Errorf("obs: encode: %w", err)
		}
	}
	if buf.Len() == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushURL, &buf)
	if err != nil {
		return fmt.Errorf("obs: push request: %w", err)
	}
	req.Header.Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("obs: push: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("obs: push: status %d", resp.StatusCode)
	}
	return nil
}
