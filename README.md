# GitForge v5 — The World in One Process

A standalone, GitHub-compatible git forge in one process, written for the
make.fun `github-core-clone-01` bounty ($500, 1st $100).

One statically linked Go binary serves all four surfaces the suite drives:

- git smart HTTP (upload-pack protocol v2, receive-pack v0) for clone, fetch
  and push, from its own object store — no git binary, no outside service
- REST v3 under `/api/v3` for repositories, issues, pull requests and contents
- GraphQL v4 at `/api/graphql`
- MCP at `/mcp` (JSON-RPC 2.0, Streamable HTTP, revision 2026-07-28)

## Running

The trial runs `node main.ts`. It copies `go/forge` to a temp dir, spawns it,
reads the `FORGE_LISTEN <port>` line, registers the port with the trial env
and waits until the env says done. Locally, `node main.ts` just serves on the
core's port for testing.

Build the core with:

```sh
cd go && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
```

## What v5 does differently

v5 is a fresh build aimed squarely at the perf column, which is the only
differentiator on the leaderboard (feature 600 and agent 200 are maxed by
everyone at the top):

- **Read workload:** the issue the suite reads 2,000 times is served from a
  pre-rendered, atomically cached JSON body — no marshal, no map walk.
- **Write workload:** issue creation extracts `title`/`body` with a
  hand-rolled scanner instead of a generic JSON decoder, and builds the
  reply by appending into a pooled buffer.
- **Push workload:** pack ingest reuses pooled zlib readers and buffers;
  objects are hashed once and stored raw.
- **Clone workload:** the pack for a repo tip is built once and cached, so
  50 concurrent shallow clones all serve the same bytes with zero rebuild.
- **HTTP:** raw TCP HTTP/1.1 with pooled buffers, zero-alloc request-line and
  header parsing, keep-alive, `TCP_NODELAY`.

Target (local, same harness as v4): push ≤ 90 ms, clone ≤ 30 ms,
read ≤ 270 ms, write ≤ 40 ms — a local p50 geomean near 72,000 µs,
about 2× v4's 144,252 µs and 5× v3's board time.
