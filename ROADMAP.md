# ROADMAP.md - tatara-memory-repo-ingester

Shipped: MVP (walker + 5 analyzers + docs + push), M2-B cross-repo
provides/requires (Go/Python/JS), Go tree-sitter fallback for non-buildable
packages, M5 SCIP v1 (`--scip` intra-repo graph ingestion).

Open:
- M5 SCIP cross-repo: parse import/export monikers into cross_repo_symbols
  provides/requires (v1 is intra-repo only).
- M5 SCIP: validate/fix reference-edge attribution against a real scip-go index
  (line-containment heuristic drops body refs when def ranges are name-token-only;
  consider SCIP enclosing_range or document symbol structure).
- Go fallback packages emit provides but not requires (no type resolution for
  external refs) - revisit if cross-repo coverage of broken packages matters.
- Prometheus Pushgateway emitter for batch counts.
- Deploy-time: Keycloak service-account client; Harbor image + infra-helmfile
  tatara-bucket Job release (deploy from main only, rule 10).
