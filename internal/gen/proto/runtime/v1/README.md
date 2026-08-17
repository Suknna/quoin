# Generated runtime.proto Go stubs

Regenerate with (from repository root, after editing the frozen
`docs/specs/quoin-v1/contracts/runtime.proto`):

```bash
protoc -I docs/specs/quoin-v1/contracts \
  --go_out=. --go_opt=module=github.com/Suknna/quoin \
  '--go_opt=Mruntime.proto=github.com/Suknna/quoin/internal/gen/proto/runtime/v1;runtimev1' \
  --go-grpc_out=. --go-grpc_opt=module=github.com/Suknna/quoin \
  '--go-grpc_opt=Mruntime.proto=github.com/Suknna/quoin/internal/gen/proto/runtime/v1;runtimev1' \
  docs/specs/quoin-v1/contracts/runtime.proto
```

The M flag redirects the frozen `go_package` (`github.com/Suknna/quoin/gen/runtime/v1`)
into `internal/gen/proto/runtime/v1` so generated code stays inside `internal/gen/**`
without editing the frozen contract. Never edit these files by hand.
