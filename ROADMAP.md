# ROADMAP.md - tatara-memory-repo-ingester

Shipped: MVP (walker + 5 analyzers + docs + push), M2-B cross-repo
provides/requires (Go/Python/JS), Go tree-sitter fallback for non-buildable
packages, M5 SCIP v1 (`--scip` intra-repo graph ingestion), 0.2.6 bulk-repo
contract fix (repo field in /memories:bulk body), Prometheus metrics pushed to
the operator pushmetrics receiver at job end (`internal/obs`, `METRICS_PUSH_URL`).

Shipped (cont.): bounded `/code-graph:bulk` batching (#31) - the graph push is
split into file-atomic batches (2000 rows / 250 files) sent sequentially, with
`Retry-After`-aware backoff on 429/503, so a whole-repo push no longer holds one
tatara-memory Postgres transaction for minutes (tatara-memory#82).

Open:
- Async graph-push pathway: mirror `/memories:bulk`'s 202 + `/ingest-jobs/{id}`
  contract for `/code-graph:bulk` (issue #31 options 1b/3b) so the server owns
  back-pressure end to end. Needs a coordinated tatara-memory change; batching
  (#31, shipped) is the client-side half.
- M5 SCIP cross-repo: parse import/export monikers into cross_repo_symbols
  provides/requires (v1 is intra-repo only).
- M5 SCIP: validate/fix reference-edge attribution against a real scip-go index
  (line-containment heuristic drops body refs when def ranges are name-token-only;
  consider SCIP enclosing_range or document symbol structure).
- Go fallback packages emit provides but not requires (no type resolution for
  external refs) - revisit if cross-repo coverage of broken packages matters.
- Deploy-time: Keycloak service-account client; Harbor image + infra-helmfile
  tatara-bucket Job release (deploy from main only, rule 10).
- Drop the kubectl dependency: report the ingested HEAD without `kubectl` in the
  image (write SHA to the Pod termination-log and have the operator read it, or
  have the operator resolve HEAD from the SCM API). Removes the 0.2.2 kubectl
  bundle and keeps the image lean.
