# Archive HTTP API

`gmcli archive serve` exposes the same renderer-independent query layer used by
the archive CLI. Queries are read-only; the explicit sync endpoint runs the
normal relay sync, atomically replaces the JSONL export, and incrementally
refreshes the disposable SQLite/FTS5 cache. JSONL remains authoritative.

```sh
gmcli archive serve \
  --dir ~/Backups/gmcli/latest \
  --listen 127.0.0.1:7878
```

The listener is restricted to a loopback address. Set
`GMCLI_ARCHIVE_API_TOKEN` before starting the server to require
`Authorization: Bearer <token>` on every request. Responses are JSON, error
responses have the shape `{"error":"..."}`, and all responses use
`Cache-Control: no-store`.

## Version 1 endpoints

| Endpoint | Query parameters | Result |
| --- | --- | --- |
| `GET /healthz` | — | Process readiness |
| `GET /api/v1/meta` | — | Archive counts, export time, and cache path |
| `GET /api/v1/conversations` | `query`, `sort=recent\|messages`, `limit`, `offset` | Conversation page |
| `GET /api/v1/conversations/{id}` | — | One conversation |
| `GET /api/v1/conversations/{id}/messages` | `limit`, `before`, or `after` | Chronological message page |
| `GET /api/v1/search` | `query`, `conversation_id`, `limit`, `offset` | Full-text search page |
| `GET /api/v1/conversations/{id}/messages/{message_id}/context` | `before`, `after` | Exact message with surrounding context |
| `POST /api/v1/sync` | — | Sync relay state, refresh JSONL and cache, and return new counts |

Message cursors are opaque. Clients should pass `before_cursor` or
`after_cursor` back unchanged and use `has_older`/`has_newer` to decide whether
another page exists. `before` and `after` are mutually exclusive.

Only one sync may run at a time. A concurrent request receives `409 Conflict`.
Sync never sends a message, but it does require the paired phone to be online.

`gmcli-viewer` starts this server as a private child process on a random
loopback port with a fresh bearer token. It can instead connect to an existing
server with `gmcli-viewer --api http://127.0.0.1:7878`.
