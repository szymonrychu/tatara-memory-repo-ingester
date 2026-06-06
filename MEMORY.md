# MEMORY.md - tatara-memory-repo-ingester

- 2026-06-06: New repo (phase 3 sub-project B). Walks a repo, emits the code
  graph + semantic chunks to tatara-memory. Spec in the parent tatara repo at
  docs/superpowers/specs/2026-06-06-tatara-memory-repo-ingester-design.md.
- 2026-06-06: No /metrics endpoint (rule 13). Batch tool with no long-running
  process to scrape; counts are emitted as a structured slog line. Rationale per
  rule 4.
- 2026-06-06: Stateless change detection. Caller supplies --since <commit>;
  querying tatara-memory for "last commit per repo" would couple B to A's
  internal state, which A does not expose.
- 2026-06-06: contract types are mirrored from tatara-memory's internal/codegraph
  (cannot import an internal package across modules); contract_shape_test.go
  guards the JSON shapes against drift.
- 2026-06-06: analyzers MUST emit repo-relative paths (filepath.Rel(absRepoRoot, absFile)) and filter all entity/edge/chunk emission to the files arg - tatara-memory /code-graph:bulk rejects any push where FilePath or SrcFile is not in the push's files set.
- 2026-06-06: Python M3 resolution ladder complete. Analyze() does a two-pass: first pass parses all files and builds a repo-wide name->[]entityID index; second pass resolves calls via scoped(0.85)->imported(0.7)->global(0.45)->ambiguous(0.2)->dangling. Decorated functions (decorated_definition node) get degraded_by=decorator and confidence capped at 0.45. Dead sitter.NewParser() allocation removed. import_from_statement tracked for imported_name_match; bare import_statement skipped (calls via module.attr are attribute nodes -> dangling).
