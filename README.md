# tatara-memory-repo-ingester

Phase 3 sub-project B of the tatara platform. A stateless Go batch tool that walks
a repository, runs best-per-language static analysis (Go, Python, JavaScript,
Terraform, Helm, plus a docs/markdown chunk path), and pushes a deterministic code
graph and enriched semantic chunks to a running `tatara-memory`. Adding a language
touches only one file in `internal/analyze` (the modularity contract).

## Usage

```
tatara-ingest --repo-root <path> [--repo-name <n>] [--since <commit>] [--full] [--base-url <url>]
```

Design and plan live in the parent `tatara` repo under
`docs/superpowers/specs/2026-06-06-tatara-memory-repo-ingester-design.md` and
`docs/superpowers/plans/2026-06-06-tatara-memory-repo-ingester.md`.
