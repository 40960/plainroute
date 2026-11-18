# SPDX-License-Identifier: AGPL-3.0-only

FROM golang:1.27-alpine AS build

WORKDIR /src

# The module files first, so a source change does not invalidate the dependency layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/plainroute ./cmd/plainroute

FROM alpine:3.20

LABEL org.opencontainers.image.title="plainroute" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.description="A small, boring, transparent OpenAI-compatible LLM gateway"

RUN apk add --no-cache ca-certificates

COPY --from=build /out/plainroute /usr/local/bin/plainroute

# The gateway needs no write access anywhere: config comes in read-only, and nothing
# is written at runtime.
RUN mkdir -p /etc/plainroute && addgroup -S plainroute && adduser -S plainroute -G plainroute
USER plainroute

EXPOSE 8080

# The health check is the live endpoint, not a shell: a container that cannot answer
# /healthz is not serving. Readiness is checked by the orchestrator, not here, because
# readiness is what a target decides and an upstream cooldown must not fail it.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O - http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/plainroute"]
CMD ["-config", "/etc/plainroute/config.yaml"]
