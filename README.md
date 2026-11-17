[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/lianajwa/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/lianajwa/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01)

# git-forge

A GitHub-compatible git forge in one Node 22 process, standard library only. It serves
git smart HTTP, the REST v3 API, GraphQL and MCP from a single in-memory store, so every
surface sees the same repositories, issues and pull requests.

## Run

```sh
node main.ts
```

Without `MAKEFUN_ENV_URL` it serves on `http://127.0.0.1:8080` (or `$PORT`) for local
testing. In a make.fun trial it binds an ephemeral port, POSTs `{"port": N}` to
`$MAKEFUN_ENV_URL/register`, then polls `$MAKEFUN_ENV_URL/done` until the trial ends
and exits 0. `MAKEFUN_SHOP_TOKEN` is the Bearer token every surface except `/healthz`
expects; when it is unset, any non-empty Bearer token is accepted (local mode only).

## What it implements

- **Git smart HTTP** (`/{owner}/{repo}.git`): protocol v0 and v2 advertisements,
  `ls-refs` and `fetch` (including shallow/deepen, peel, symrefs, unborn), and
  `receive-pack` pushes with per-ref `ok`/`ng` reports and `non-fast-forward` refusals.
  Packs are written without deltas and read back with full OFS/REF delta resolution.
- **REST v3** (`/api/v3`): repositories (create/read/update/delete, paginated list),
  branches, commits, git refs, contents (create/update/delete with base64 and stale-sha
  409s), labels, issues, issue comments, pull requests (create/list/files) and
  squash merges with conflict detection (405). Every reply is `application/json` with a
  non-empty `X-GitHub-Request-Id`; errors carry a JSON `{message}`.
- **GraphQL** (`/api/graphql`): viewer, repository (with issues/pullRequests connections,
  cursors, aliases and fragments), node by global id, mutations (create/close issue,
  create/merge pull request, add comment), `__type` introspection, and batch requests.
  Unknown fields answer HTTP 200 with an `errors` array naming the field.
- **MCP** (`/mcp`, JSON-RPC 2.0, era 2026-07-28): `server/discover`, `initialize`,
  `tools/list` and `tools/call` for `create_repo`, `get_repo`, `create_issue`,
  `list_issues`, `create_pull_request` and `merge_pull_request`. Tool results carry both
  `structuredContent` and a JSON text block.

## Layout

```
main.ts            entrypoint: bind, register with the env, wait for done
src/gitcore.ts     git objects, pkt-lines, trees, commits, packs, reachability
src/store.ts       the Forge: repositories, refs, contents, issues, pulls, merges
src/git-http.ts    smart-HTTP handlers (info/refs, upload-pack, receive-pack)
src/rest.ts        REST v3 router and JSON views
src/graphql.ts     GraphQL lexer, parser, validator and executor
src/mcp.ts         MCP JSON-RPC server and the six tools
src/server.ts      the HTTP server: routing, auth, request ids, body limits
```

## Determinism

All state lives in memory and nothing in request handling reads the wall clock or an
unseeded random source: server-created commits use a fixed identity and timestamp,
request ids come from a monotonic counter, and global ids are base64 of typed keys. A
trial replay therefore leaves a byte-identical state export.

## License

MIT — see `LICENSE`.
