# MEMORY.md - tatara-memory-repo-ingester

- 2026-06-14: Added .mise.toml (go=1.25.0, golangci-lint=2.12.2, CGO_ENABLED=1). CI replaced setup-go+golangci-lint-action with jdx/mise-action@v2 + mise run tasks. System gcc (build-essential) still required in test/build/smoke CI jobs - mise does not provide gcc and the ingester needs cgo for tree-sitter. lint job has no apt step (golangci-lint is pure binary).
- 2026-06-12 (0.2.6): Fixed reingest Job 400 from /memories:bulk: BulkMemoriesRequest was missing the `repo` field (omitempty, omitted when blank), and PushChunks built the body without setting it. Memory API requires `repo` when `reconcile_files` is non-empty. Fix: added `Repo string json:"repo,omitempty"` to BulkMemoriesRequest, added `repo string` first arg to PushChunks, updated run.go call site to pass `o.repoName`. Chart appVersion bumped 0.1.0->0.2.6 (was stale vs deployed 0.2.5).
- 2026-06-08 (0.2.2): Bundled `kubectl` (pinned v1.33, the cluster minor) into the
  runtime image. The operator's ingest Job runs `tatara-ingest && kubectl patch
  configmap <result> -p {sha}` (in-cluster SA auth) to report the ingested HEAD;
  the operator reads that SHA to flip the Repository to `Ingested`. The image had
  no kubectl, so the patch failed `kubectl: not found` AFTER a successful ingest
  -> Job exited non-zero -> repo never recorded its commit, stuck `Ingesting`,
  operator relaunched in a loop. Side effects (graph + chunks) already landed;
  only the report-back failed. Surfaced only once the chunk path drained end-to-
  end (tatara-memory 0.2.2/0.2.3 fixes). Cleaner long-term: ingester self-reports
  HEAD via the Pod termination-log (no kubectl in image) or the operator resolves
  HEAD from the SCM API - see ROADMAP.
- 2026-06-08 (0.2.1): Runtime binary moved from `/tatara-ingest` to
  `/usr/local/bin/tatara-ingest` (on PATH); ENTRYPOINT now bare `tatara-ingest`.
  The operator's ingest Job runs `/bin/sh -c "tatara-ingest ..."` (bare name via
  PATH); the root-level binary gave exit 127 `tatara-ingest: not found` on the
  first real dogfood ingest. Surfaced only at runtime under the operator (the
  image's own ENTRYPOINT worked, masking it).

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
- 2026-06-06: JS analyzer TDD gaps closed: require() import edge emission (processFile emits RelImports for js:module: importMap values), unresolved-tier dangling_call assertion, and degraded/dynamic both pinned as explicit assertions. jsCollectRequireImports switched to O(1) moduleSet map instead of O(n) repoIndex value scan.
- 2026-06-06: Go method requires join-key bug fixed. crossRepoSymbolName() reconstructs <RecvType>.<Method> so requires key matches provides key byte-for-byte. goObjKind() now returns "method" for *types.Func with receiver. Two-module fixture (dep+replace) guards regression in TestGoRequiresEmission.
- 2026-06-06: Python M3 resolution ladder complete. Analyze() does a two-pass: first pass parses all files and builds a repo-wide name->[]entityID index; second pass resolves calls via scoped(0.85)->imported(0.7)->global(0.45)->ambiguous(0.2)->dangling. Decorated functions (decorated_definition node) get degraded_by=decorator and confidence capped at 0.45. Dead sitter.NewParser() allocation removed. import_from_statement tracked for imported_name_match; bare import_statement skipped (calls via module.attr are attribute nodes -> dangling).
- 2026-06-06: M5 v1 SCIP ingest implemented. --scip <index.scip> --scip-repo <name> parses a pre-generated SCIP protobuf and pushes intra-repo graph only (no chunks). SCIP import/export monikers for cross-repo provides/requires deferred to ROADMAP.
- 2026-06-06: Go tree-sitter fallback added. When go/packages reports pkg.Errors > 0, fallbackAnalyzeGoPackage() parses in-scope files with github.com/smacker/go-tree-sitter/golang, emits go_func/go_method entities and scoped name-match calls edges capped at confidence 0.45 with degraded_by=no_typecheck. Fallback packages emit provides SymbolRows (names visible) but NOT requires (no type resolution to attribute external refs). pkgPath computed structurally from go.mod module path + file dir relative to repo root.
- 2026-06-06: M5 SCIP v1 LIMITATION - reference edges are attributed to the enclosing
  definition by line-range containment. Real SCIP definition occurrences often range
  over the name token only (single line), not the function body, so reference edges
  inside a body may not attribute and can drop. Entities (one per definition) are solid;
  reference-edge coverage needs validation against a real scip-go index. Tracked in ROADMAP.
- 2026-06-07: Runtime image swapped from distroless/cc-debian12:nonroot to golang:1.26-bookworm. Distroless had no git/go; ingest Jobs need to clone repos and run scip-go (Go toolchain). ENV GOTOOLCHAIN=auto added so ingested repos pinned to a newer Go still work. USER nonroot dropped (pod securityContext sets runAsUser).
