# syntax=docker/dockerfile:1.7

# ─── build stage ───────────────────────────────────────────────────────────
# Build still happens on Alpine because the daemon binary is CGO-disabled
# pure Go — libc doesn't matter here. Smaller cache layer, faster CI.
FROM golang:1.23-alpine AS build

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/agent-opsd ./cmd/agent-opsd

# ─── runtime stage ─────────────────────────────────────────────────────────
# Switched off Alpine (musl) to Debian (glibc) so catalog scripts can install
# the entire `manylinux` Python/Node ecosystem — PyTorch / playwright /
# onnxruntime / sentence-transformers etc all ship glibc-only wheels with
# no Alpine equivalent. node:20-bookworm-slim already ships Node 20 + npm
# so we don't need a separate NodeSource setup step.
FROM node:20-bookworm-slim

# `apt-get install` defaults to interactive prompts; non-interactive lets
# `tzdata` and friends install silently.
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        curl \
        bash \
        procps \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g --no-audit --no-fund @anthropic-ai/claude-code \
 && mkdir -p /var/lib/agent-ops /etc/agent-ops

COPY --from=build /out/agent-opsd /usr/local/bin/agent-opsd

COPY config.example.yaml /etc/agent-ops/config.yaml

# Claude Code CLI's Bash tool requires /bin/bash explicitly; SHELL=/bin/bash
# is on Debian by default but we set it for parity with the old Alpine image.
ENV SHELL=/bin/bash

# Catalog install scripts need root for apt-get / mount / etc. The container
# is single-tenant (one Fargate task per managed-app instance) so the
# isolation boundary is the container, not the in-container UID.
USER root
WORKDIR /root

EXPOSE 7842

HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:7842/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/agent-opsd"]
CMD ["--config", "/etc/agent-ops/config.yaml"]
