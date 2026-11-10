ARG BASE_IMAGE_REGISTRY=cgr.dev
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE_REGISTRY}/chainguard/go:latest AS builder
ARG TARGETARCH
ARG TARGETOS

WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod,rw \
    --mount=type=cache,target=/root/.cache/go-build,rw \
    go mod download
COPY api/ api/
COPY adk/ adk/
COPY core/ core/
COPY harness/ harness/
COPY pkg/ pkg/
RUN --mount=type=cache,target=/root/go/pkg/mod,rw \
    --mount=type=cache,target=/root/.cache/go-build,rw \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -o /kagent-claude ./harness/claude/cmd

# The Agent SDK bridge (KAGENT_CLAUDE_DRIVER=sdk) is plain JavaScript. Its
# platform CLI packages are optional dependencies and are left out: the bridge
# drives the image's own pinned, validated claude.
FROM --platform=$BUILDPLATFORM node:22-alpine AS sdk-runner
WORKDIR /sdk-runner
COPY harness/claude/sdk-runner/package.json harness/claude/sdk-runner/package-lock.json ./
RUN npm ci --omit=optional --omit=dev --ignore-scripts --no-audit --no-fund
COPY harness/claude/sdk-runner/runner.mjs ./

FROM alpine:3.22
ARG TARGETARCH
ARG VERSION

COPY harness/runtime/payload/install-cli.sh /build/install-cli
COPY harness/runtime/payload/runtime-lock.json /build/runtime-lock.json

ENV USE_BUILTIN_RIPGREP=0
RUN apk upgrade --no-cache && apk add --no-cache bash ca-certificates git libgcc libstdc++ ripgrep jq nodejs \
    && /build/install-cli claude "$TARGETARCH" /usr/local /build/runtime-lock.json \
    && rm -rf /build \
    && addgroup -S -g 65532 kagent \
    && adduser -S -D -H -u 65532 -G kagent -h /data -s /sbin/nologin kagent \
    && mkdir -p /data/adapter /data/claude /data/generated /data/workspace \
    && chown -R 65532:65532 /data

COPY --from=builder /kagent-claude /usr/local/bin/kagent-claude
COPY --from=sdk-runner /sdk-runner /opt/kagent/sdk-runner

USER 65532:65532
WORKDIR /data/workspace
EXPOSE 80 8081 7682

LABEL org.opencontainers.image.source="https://github.com/kagent-dev/kagent"
LABEL org.opencontainers.image.description="Kagent Claude native Harness adapter"
LABEL org.opencontainers.image.version="$VERSION"

ENTRYPOINT ["/usr/local/bin/kagent-claude"]
