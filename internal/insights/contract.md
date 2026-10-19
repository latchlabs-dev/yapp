# Insights gRPC contract snapshot

The generated client in this directory is a build-local snapshot of the canonical contract in the separately versioned `piproxy-insights` repository:

`../piproxy-insights/api/insights/v1/insights.proto`

The current snapshot was generated from service commit `e9acab8`.

PiProxy intentionally does not depend on a sibling checkout or an unpublished Go module during production builds. To refresh the snapshot after a compatible contract change:

```bash
protoc -I ../piproxy-insights/api \
  --go_out=. --go_opt=module=github.com/earlgrey/piproxy \
  --go_opt=Minsights/v1/insights.proto=github.com/earlgrey/piproxy/internal/insightsrpc/v1\;insightsv1 \
  --go-grpc_out=. --go-grpc_opt=module=github.com/earlgrey/piproxy \
  --go-grpc_opt=Minsights/v1/insights.proto=github.com/earlgrey/piproxy/internal/insightsrpc/v1\;insightsv1 \
  ../piproxy-insights/api/insights/v1/insights.proto
```

The boundary accepts only PiProxy-parsed transcript items and returns optional summaries, tagged insights, optional proposed tags, model usage, and warnings. Tenant identity, raw session formats, queues, persistence, and Duffel are not part of the service contract.
