# httpserver

`httpserver` serves a `core.Runtime` over `net/http`. It has no lifecycle
state of its own: every response is read from the Runtime's committed store,
so clients can submit, disconnect, reconnect to any process sharing the store,
and resume an event stream exactly where it stopped.

```go
server, err := httpserver.New(httpserver.Config{
	Runtime:   runtime,
	Authorize: authorize, // required; return httpserver.ErrUnauthenticated for 401
	Scope:     tenantOf,  // optional; isolates tenants by idempotency scope
	Actor:     actorOf,   // audit identity on wait answers
})
http.Handle("/", server)
```

| Route | Purpose |
| --- | --- |
| `POST /v1/runs` | Admit a run (`definition`, `task`, `idempotency_key`, `deadline`, `labels`, `conversation`) |
| `GET /v1/runs` | List runs (`definition`, `revision`, `state`, `attention`, `label=k:v`, `roots`, `created_after`, `created_before`, `after`, `limit`) |
| `GET /v1/runs/{id}` | Run status, waits, and hook outcomes |
| `GET /v1/runs/{id}/events` | Committed events as a JSON page, or SSE with `Accept: text/event-stream` |
| `POST /v1/runs/{id}/cancel` | Cancel a run and its descendants |
| `POST /v1/runs/{id}/waits/{wait}` | Answer a question or decide an approval |
| `POST /v1/runs/{id}/operations/{op}/reconcile` | Settle an uncertain tool operation (listed as `uncertain_operations` on the run) with the destination's answer; the tool never runs again |
| `POST /v1/runs/{id}/hooks/acknowledge` | Acknowledge interrupted committed-run hook delivery; the hooks never run again |
| `GET /v1/conversations/{id}` | Conversation head (the next turn's `expected_head`), active turn, and turn count, within the request's tenant scope |
| `GET /v1/changes` | Runtime-wide change feed (single-tenant servers only), JSON or SSE; `events=true` attaches events |
| `GET /v1/definitions/{id}/{revision}` | Definition manifest |
| `GET /v1/openapi.json` | This API's OpenAPI 3.1 document (also `httpserver.OpenAPI()`); served without authorization because it carries no run data |

Streams resume from `Last-Event-ID` (or `after`), send a heartbeat comment
when idle, and a run stream ends with an `end` event after the run's terminal
state. Events are read from storage on demand, so a slow client falls behind
without buffering in the server or delaying runs. A cursor that retention
removed answers `410 cursor_not_retained`; resynchronize from the run status.

Content is redacted by default: status, events, and manifests carry only
lifecycle facts until `ExposeContent` is set. Request bodies, concurrent
streams, and stream duration are bounded by `MaxBodyBytes`, `MaxStreams`, and
`MaxStreamDuration`. Every stream write (including the initial flush) has a
30-second deadline, shortened to the remaining stream duration; cancellation
interrupts blocked writes and releases the stream slot. Middleware wrapping a
response writer must preserve write-deadline support, for example through an
`Unwrap() http.ResponseWriter` method; otherwise streaming answers 501.

Operator commands check tenant ownership from the compact run record, not its
transcript, so damaged history cannot prevent cancellation, reconciliation,
wait answers, or hook acknowledgement. A successful command whose full status
cannot be read returns compact status with `history_unavailable: true`.
Full status and event reads report `500 payload_unavailable` for damaged
history. Neither path weakens tenant isolation.
