# ROADMAP.md - tatara-memory-repo-ingester

- M2 cross-repo linking (emit provides/requires; needs A-side cross_repo_symbols
  table + join). Fast-follow, spans A and B.
- M5 SCIP cross-repo: parse import/export monikers into cross_repo_symbols provides/requires (v1 landed, intra-repo only).
- tree-sitter fallback for non-buildable Go packages.
- Prometheus Pushgateway emitter for batch counts.
- Deploy-time: Keycloak service-account client; Harbor image + infra-helmfile
  tatara-bucket Job release (deploy from main only, rule 10).
- M5 SCIP: validate/fix reference-edge attribution against a real scip-go index
  (line-containment heuristic drops body refs when def ranges are name-token-only;
  consider SCIP enclosing_range or document symbol structure).
