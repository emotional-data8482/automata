# Automata maintenance map

## Runtime path

Callers finish mutating `core.Agent` configuration before any runs start; each
`Run` then creates a fresh loop. `Session` serializes calls and carries the
canonical transcript. The loop applies pre-send hooks, invokes the provider
with retries, commits the assistant response, executes tool batches, commits
one result per call, and returns a populated `RunResult` on success or failure.

Streaming uses the same lifecycle while emitting serialized callbacks.
`StreamAccumulator` groups events by `(Agent, InvocationID)` so nested and
repeated sub-agent calls remain distinct.

## Change impact

| Change | Inspect together |
| --- | --- |
| Message or block model | `core/blocks.go`, `core/types.go`, JSON tests, every provider converter |
| Run lifecycle or errors | `core/loop.go`, `core/agent.go`, `core/session.go`, hooks, streaming tests |
| Tool execution | `core/tools.go`, `core/toolbatch.go`, approvals, policy, rich results, nested agents |
| Streaming | `core/stream.go`, `core/emitter.go`, accumulator, both provider stream implementations |
| Typed output | `core/typed.go`, schema derivation/validation, provider capability mapping |
| Provider options | `core/provider.go`, Claude and OpenAI request builders, docs/examples |
| First-party tool API | `tools`, affected backend extension, security bounds, consumer examples |
| Module/version change | every published `go.mod`, `go.work`, examples, README release contract |

## Verification ladder

Run the narrowest useful checks first:

```sh
go test ./core
go test -race ./core
```

For a nested module, run from that module directory:

```sh
go test ./...
GOWORK=off go test ./...
```

For a cross-cutting change, test the root plus every affected module. Build
affected examples without credentials. Live provider calls are opt-in
integration checks and must never be required for ordinary unit validation.
