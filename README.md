# tatara-memory-repo-ingester

Phase 3 sub-project B of the tatara platform. A stateless Go batch tool that walks
a repository, runs best-per-language static analysis (Go, Python, JavaScript,
Terraform, Helm, plus a docs/markdown chunk path), and pushes a deterministic code
graph and enriched semantic chunks to a running `tatara-memory`. Adding a language
touches only one file in `internal/analyze` (the modularity contract).

## Usage

Walk-based ingest (source repo):

```
tatara-ingest --repo-root <path> [--repo-name <n>] [--since <commit>] [--full] [--base-url <url>]
```

SCIP-index ingest (pre-generated `index.scip`, bypasses the source walker):

```
tatara-ingest --scip <index.scip> --scip-repo <name> [--base-url <url>]
```

`--scip-repo` is required when `--scip` is set. Only the code graph is pushed
(no semantic chunks in v1); entities and edges come directly from the SCIP index.

Design and plan live in the parent `tatara` repo under
`docs/superpowers/specs/2026-06-06-tatara-memory-repo-ingester-design.md` and
`docs/superpowers/plans/2026-06-06-tatara-memory-repo-ingester.md`.
