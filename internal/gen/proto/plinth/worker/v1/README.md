# Generated agent_worker.proto Go stubs

Regenerate with (from repository root; the frozen contract is
`docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1/agent_worker.proto`):

```bash
protoc -I docs/specs/quoin-v1/contracts \
  --go_out=. --go_opt=module=github.com/Suknna/quoin \
  '--go_opt=Mquoin/plinth/worker/v1/agent_worker.proto=github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1;workerv1' \
  '--go_opt=Mruntime.proto=github.com/Suknna/quoin/internal/gen/proto/runtime/v1;runtimev1' \
  docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1/agent_worker.proto
```

The M flags redirect the frozen `go_package` declarations into
`internal/gen/proto/**` so generated code stays inside `internal/gen/**`
without editing the frozen contract. Never edit these files by hand.
