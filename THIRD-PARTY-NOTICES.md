# Third-party notices

mrtc-sip-bridge is licensed under the MIT License (see `LICENSE`). It links the following third-party
software. Each component remains under its own license; the full license texts are in the respective
upstream repositories (and in the Go module cache for the exact versions pinned in `go.mod`). Licenses
were checked against upstream repositories; see `docs/CREDITS.md` for notes.

MPL-2.0 components (mautrix, go.mau.fi/util, diago) are used **unmodified** and linked as Go modules.
Their source is available at the URLs below.

## Direct runtime dependencies

| Component | Version | License | Source |
|---|---|---|---|
| maunium.net/go/mautrix (bridgev2) | v0.33.0 | MPL-2.0 | https://github.com/mautrix/go |
| go.mau.fi/util | v0.11.1 | MPL-2.0 | https://github.com/mautrix/go-util |
| emiago/diago | v0.41.0 | MPL-2.0 | https://github.com/emiago/diago |
| emiago/sipgo | v1.6.0 | BSD-2-Clause | https://github.com/emiago/sipgo |
| livekit/server-sdk-go/v2 | v2.18.1 | Apache-2.0 | https://github.com/livekit/server-sdk-go |
| livekit/protocol | v1.49.0 | Apache-2.0 | https://github.com/livekit/protocol |
| pion/webrtc/v4 | v4.2.15 | MIT | https://github.com/pion/webrtc |
| pion/rtp | v1.10.2 | MIT | https://github.com/pion/rtp |
| pion/stun/v3 | v3.1.7 | MIT | https://github.com/pion/stun |
| hraban/opus (gopkg.in/hraban/opus.v2) | v2.0.0-20230925203106-0188a62cb302 | MIT | https://github.com/hraban/opus |
| libopus (xiph/opus), linked via cgo | system package | BSD-3-Clause | https://github.com/xiph/opus |
| rs/zerolog | v1.35.1 | MIT | https://github.com/rs/zerolog |
| go-logr/logr | v1.4.4 | Apache-2.0 | https://github.com/go-logr/logr |
| gopkg.in/yaml.v3 | v3.0.1 | MIT and Apache-2.0 | https://github.com/go-yaml/yaml |
| OpenTelemetry Go (otel, otel/trace, otel/metric, otel/sdk, otel/sdk/metric, OTLP trace and metric exporters) | v1.47.0 | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go |
| OpenTelemetry Go contrib (instrumentation/runtime) | v0.72.0 | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go-contrib |

## Notable indirect dependencies

Transitive dependencies (see `go.mod`/`go.sum`) are under permissive licenses, mainly MIT, BSD-2/3-Clause
and Apache-2.0. Notable ones: grpc/grpc-go (Apache-2.0), google.golang.org/protobuf (BSD-3-Clause),
grpc-ecosystem/grpc-gateway (BSD-3-Clause), cenkalti/backoff (MIT), open-telemetry/opentelemetry-proto-go
(Apache-2.0), and other pion modules (MIT). Run `go-licenses report ./...` to regenerate a
complete list.

## libopus license (BSD-3-Clause)

Copyright 2001-2023 Xiph.Org, Skype Limited, Octasic, Jean-Marc Valin, Timothy B. Terriberry,
CSIRO, Gregory Maxwell, Mark Borgerding, Erik de Castro Lopez, Microsoft Corporation, Amazon.
Redistribution and use in source and binary forms, with or without modification, are permitted provided
the copyright notice, this list of conditions and the disclaimer in the upstream COPYING file are
retained. See https://github.com/xiph/opus/blob/main/COPYING.
