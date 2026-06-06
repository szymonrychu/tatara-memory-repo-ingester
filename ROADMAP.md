# ROADMAP.md - tatara-memory-repo-ingester

- M2 cross-repo linking (emit provides/requires; needs A-side cross_repo_symbols
  table + join). Fast-follow, spans A and B.
- M5 SCIP interchange analyzer (Java/C++/TS via off-the-shelf indexers).
- tree-sitter fallback for non-buildable Go packages.
- Prometheus Pushgateway emitter for batch counts.
- Deploy-time: Keycloak service-account client; Harbor image + infra-helmfile
  tatara-bucket Job release (deploy from main only, rule 10).
