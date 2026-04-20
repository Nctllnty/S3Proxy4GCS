# Autonomous Agent Context (AGENTS.md)

This file contains instructions and context for AI coding assistants working on the `s3proxy4gcs` repository.

## Project Vision

The goal is to serve as a transparent middleware for S3 protocols to translate unsupported features into GCS APIs seamlessly.

## Engineering Rules

1.  **Zero Tolerance for Syntax Errors**: Before committing or saving, ensure bracket matching and interface compliance is correct.
2.  **Centralized Configuration**: All environment variables and settings must be managed in `config/settings.go`. Use `.env` file for local development.
3.  **Documentation Sync**: Update `README.md` and `AGENTS.md` whenever the project footprint (ports, dependencies, paths) changes.
4.  **Reject Unsupported Filters**: Reject lifecycle rules using unsupported filters (Size, Tags) to prevent accidental over-deletion in GCS (Scope Broadening).
5.  **Full Scope Search**: Before implement translation, search official AWS S3 SDK for full parameters. Enforce strict type validation and test both valid and invalid fields.
6.  **Full Reverse Proxy**: The proxy handles all traffic by default using standard Go `httputil.NewSingleHostReverseProxy`. For data-plane operations (`GET`/`PUT` objects), ensure streaming behavior is preserved (do not read the entire body into memory). Tune `http.Transport` connection pools (`MaxIdleConns`, `MaxIdleConnsPerHost`) for high concurrency.
7.  **Context Propagation**: Always use the request's context (`r.Context()`) for outbound GCS API calls (e.g. `bucket.Update()`). If the client aborts, the outbound GCS call automatically cancels to save compute/cost.
8.  **Standard S3 Errors**: Use the `writeS3Error` helper to respond with standard AWS S3 XML error formats. Do not use plain text `http.Error` as SDK clients expect XML.
9.  **Structured JSON Logging**: When logging, use standard Go 1.21's `log/slog` module instead of standard `log.Printf`. Use semantic levels (`Info`, `Error`, `Debug`) and use keyword arguments (e.g., `slog.Info("msg", "key", val)`) to ensure parsed compatibility with Cloud Logging.
10. **Multi-Object Delete Support**: Bulk deletion via `DeleteObjects` (`POST /?delete`) is natively supported by GCS's XML API. The proxy automatically strips non-compliant client headers (e.g., `Accept-Encoding: identity`), re-signs the request using HMAC v4, and forwards the payload directly to GCS to process bulk deletes without requiring custom fan-out translation logic.

## Environment Layout

- `main.go`: Entry point for the Chi router setup.
- `config/settings.go`: Parameter load path.
- `pkg/translate`: Location for XML translation logic (implements S3 Lifecycle).
- `.env`: Secret bind template. Use `GCS_PREFIX` for test isolation.

---

## Workspace Status

The project is currently set up as a standalone Go module (`module s3proxy4gcs`).
For testing locally without breaking user paths, you can build locally with standard Go runtimes.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **S3Proxy4GCS** (652 symbols, 941 relationships, 18 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> If any GitNexus tool warns the index is stale, run `npx gitnexus analyze` in terminal first.

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `gitnexus_impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `gitnexus_detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `gitnexus_query({query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `gitnexus_context({name: "symbolName"})`.

## When Debugging

1. `gitnexus_query({query: "<error or symptom>"})` — find execution flows related to the issue
2. `gitnexus_context({name: "<suspect function>"})` — see all callers, callees, and process participation
3. `READ gitnexus://repo/S3Proxy4GCS/process/{processName}` — trace the full execution flow step by step
4. For regressions: `gitnexus_detect_changes({scope: "compare", base_ref: "main"})` — see what your branch changed

## When Refactoring

- **Renaming**: MUST use `gitnexus_rename({symbol_name: "old", new_name: "new", dry_run: true})` first. Review the preview — graph edits are safe, text_search edits need manual review. Then run with `dry_run: false`.
- **Extracting/Splitting**: MUST run `gitnexus_context({name: "target"})` to see all incoming/outgoing refs, then `gitnexus_impact({target: "target", direction: "upstream"})` to find all external callers before moving code.
- After any refactor: run `gitnexus_detect_changes({scope: "all"})` to verify only expected files changed.

## Never Do

- NEVER edit a function, class, or method without first running `gitnexus_impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `gitnexus_rename` which understands the call graph.
- NEVER commit changes without running `gitnexus_detect_changes()` to check affected scope.

## Tools Quick Reference

| Tool | When to use | Command |
|------|-------------|---------|
| `query` | Find code by concept | `gitnexus_query({query: "auth validation"})` |
| `context` | 360-degree view of one symbol | `gitnexus_context({name: "validateUser"})` |
| `impact` | Blast radius before editing | `gitnexus_impact({target: "X", direction: "upstream"})` |
| `detect_changes` | Pre-commit scope check | `gitnexus_detect_changes({scope: "staged"})` |
| `rename` | Safe multi-file rename | `gitnexus_rename({symbol_name: "old", new_name: "new", dry_run: true})` |
| `cypher` | Custom graph queries | `gitnexus_cypher({query: "MATCH ..."})` |

## Impact Risk Levels

| Depth | Meaning | Action |
|-------|---------|--------|
| d=1 | WILL BREAK — direct callers/importers | MUST update these |
| d=2 | LIKELY AFFECTED — indirect deps | Should test |
| d=3 | MAY NEED TESTING — transitive | Test if critical path |

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/S3Proxy4GCS/context` | Codebase overview, check index freshness |
| `gitnexus://repo/S3Proxy4GCS/clusters` | All functional areas |
| `gitnexus://repo/S3Proxy4GCS/processes` | All execution flows |
| `gitnexus://repo/S3Proxy4GCS/process/{name}` | Step-by-step execution trace |

## Self-Check Before Finishing

Before completing any code modification task, verify:
1. `gitnexus_impact` was run for all modified symbols
2. No HIGH/CRITICAL risk warnings were ignored
3. `gitnexus_detect_changes()` confirms changes match expected scope
4. All d=1 (WILL BREAK) dependents were updated

## Keeping the Index Fresh

After committing code changes, the GitNexus index becomes stale. Re-run analyze to update it:

```bash
npx gitnexus analyze
```

If the index previously included embeddings, preserve them by adding `--embeddings`:

```bash
npx gitnexus analyze --embeddings
```

To check whether embeddings exist, inspect `.gitnexus/meta.json` — the `stats.embeddings` field shows the count (0 means no embeddings). **Running analyze without `--embeddings` will delete any previously generated embeddings.**

> Claude Code users: A PostToolUse hook handles this automatically after `git commit` and `git merge`.

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
