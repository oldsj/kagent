# Build with go/ as the context. The final OCI image is a read-only file payload.
ARG BASE_IMAGE_REGISTRY=cgr.dev
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE_REGISTRY}/chainguard/go:latest AS builder
ARG TARGETARCH
ARG TARGETOS
ARG PROVIDER
WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go mod download
COPY api/ api/
COPY adk/ adk/
COPY core/ core/
COPY harness/ harness/
COPY pkg/ pkg/
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
 CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/kagent-$PROVIDER ./harness/$PROVIDER/cmd \
 && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/launch ./harness/runtime/payload/cmd/launch \
 && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/kagent-credential-refresh ./core/cmd/credential-refresh

# Claude's Agent SDK bridge (KAGENT_CLAUDE_DRIVER=sdk). The SDK's entry module
# is one self-contained ES module that imports only Node builtins, so it ships
# alone beside the runner: about 1 MB instead of the 60 MB npm tree. Its peer
# and platform CLI packages stay out; the bridge drives the payload's pinned
# claude. sdk_pin_test keeps the SDK, runtime-lock.json and the harness pin on
# one Claude Code release.
FROM --platform=$BUILDPLATFORM node:22-alpine AS sdk-runner
WORKDIR /sdk-runner
COPY harness/claude/sdk-runner/package.json harness/claude/sdk-runner/package-lock.json ./
RUN npm ci --omit=optional --omit=dev --ignore-scripts --no-audit --no-fund \
 && sdk=node_modules/@anthropic-ai/claude-agent-sdk \
 && mkdir -p "/out/$sdk" \
 && cp "$sdk/package.json" "$sdk/sdk.mjs" "$sdk/LICENSE.md" "/out/$sdk/"
COPY harness/claude/sdk-runner/runner.mjs /out/

FROM alpine:3.22 AS payload
ARG TARGETARCH
ARG PROVIDER
# Only the Claude payload carries Node, for the SDK bridge.
RUN apk upgrade --no-cache && apk add --no-cache bash ca-certificates git libgcc libstdc++ ripgrep jq patchelf coreutils \
    $(if [ "$PROVIDER" = claude ]; then echo nodejs; fi)
COPY harness/runtime/payload/install-cli.sh /build/install-cli
COPY harness/runtime/payload/runtime-lock.json /build/runtime-lock.json
RUN /build/install-cli "$PROVIDER" "$TARGETARCH" /opt/mainloop-runtime /build/runtime-lock.json
COPY --from=builder /out/ /opt/mainloop-runtime/bin/
COPY --from=sdk-runner /out/ /build/sdk-runner/
# Copy the Alpine helper/library closure without introducing a global LD_LIBRARY_PATH.
# Dereference symlinks: the mounted payload never resolves into D's filesystem.
# Git helpers share inodes; configure their private ABI only once.
# Codex package helpers (codex-code-mode-host) also sit beside the relocated CLI
# in libexec, since the CLI may resolve them relative to its own executable.
# Claude's Node and SDK bridge sit in libexec too, off the launcher's PATH, so
# they never shadow a development image's own node. Node is patched to the
# private loader like the other helpers. Alpine's Node uses system ICU, whose
# data file ships in share/icu; the bridge passes it with --icu-data-dir. The
# build imports the SDK through that Node, with the build's ICU moved away, as
# a smoke check.
RUN mkdir -p /opt/mainloop-runtime/lib /opt/mainloop-runtime/libexec /opt/mainloop-runtime/share \
 && mv "/opt/mainloop-runtime/bin/$PROVIDER" "/opt/mainloop-runtime/libexec/$PROVIDER" \
 && cp /opt/mainloop-runtime/bin/launch "/opt/mainloop-runtime/bin/$PROVIDER" \
 && for helper in /opt/mainloop-runtime/bin/codex-*; do \
      [ -f "$helper" ] && [ ! -e "/opt/mainloop-runtime/libexec/${helper##*/}" ] && cp "$helper" /opt/mainloop-runtime/libexec/ || true; \
    done \
 && if [ "$PROVIDER" = claude ]; then \
      cp -L /usr/bin/node /opt/mainloop-runtime/libexec/node \
      && cp -R /build/sdk-runner /opt/mainloop-runtime/libexec/sdk-runner \
      && mkdir -p /opt/mainloop-runtime/share/icu \
      && cp /usr/share/icu/*/icudt*.dat /opt/mainloop-runtime/share/icu/; \
    fi \
 && cp -L /bin/bash /usr/bin/git /usr/bin/rg /opt/mainloop-runtime/bin/ \
 && cp -RL --preserve=links /usr/libexec/git-core /opt/mainloop-runtime/libexec/ \
 && cp -RL /usr/share/git-core /opt/mainloop-runtime/share/ \
 && cp -L /lib/*.so* /usr/lib/*.so* /opt/mainloop-runtime/lib/ \
 && find /opt/mainloop-runtime -type f -exec sh -c '\
   for file do \
     case "$file" in /opt/mainloop-runtime/libexec/claude|/opt/mainloop-runtime/libexec/codex) continue ;; esac; \
     if interpreter=$(patchelf --print-interpreter "$file" 2>/dev/null); then \
       case "$interpreter" in /opt/mainloop-runtime/lib/ld-musl-*.so.1) continue ;; esac; \
       patchelf --set-interpreter /opt/mainloop-runtime/lib/ld-musl-*.so.1 --set-rpath /opt/mainloop-runtime/lib "$file"; \
     fi; \
   done' sh {} + \
 && if [ "$PROVIDER" = claude ]; then \
      cd /opt/mainloop-runtime/libexec/sdk-runner \
      && mv /usr/share/icu /usr/share/icu.build \
      && /opt/mainloop-runtime/libexec/node --icu-data-dir=/opt/mainloop-runtime/share/icu --input-type=module \
        -e "const sdk = await import('@anthropic-ai/claude-agent-sdk'); if (typeof sdk.query !== 'function') process.exit(1)" \
      && mv /usr/share/icu.build /usr/share/icu && cd /; \
    fi \
 && chmod -R a+rX /opt/mainloop-runtime \
 && /opt/mainloop-runtime/bin/launch --write-manifest "$PROVIDER"

FROM scratch
COPY --from=payload /opt/mainloop-runtime/ /
# Substrate mounts the image root at /opt/mainloop-runtime, not its image config.
LABEL org.opencontainers.image.description="Kagent native runtime payload (musl-private ABI, schema 1)"
