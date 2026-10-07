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

FROM alpine:3.22 AS payload
ARG TARGETARCH
ARG PROVIDER
RUN apk add --no-cache bash ca-certificates git libgcc libstdc++ ripgrep jq patchelf coreutils
COPY harness/runtime/payload/install-cli.sh /build/install-cli
COPY harness/runtime/payload/runtime-lock.json /build/runtime-lock.json
RUN /build/install-cli "$PROVIDER" "$TARGETARCH" /opt/mainloop-runtime /build/runtime-lock.json
COPY --from=builder /out/ /opt/mainloop-runtime/bin/
# Copy the Alpine helper/library closure without introducing a global LD_LIBRARY_PATH.
# Dereference symlinks: the mounted payload never resolves into D's filesystem.
RUN mkdir -p /opt/mainloop-runtime/lib /opt/mainloop-runtime/libexec /opt/mainloop-runtime/share \
 && mv "/opt/mainloop-runtime/bin/$PROVIDER" "/opt/mainloop-runtime/libexec/$PROVIDER" \
 && cp /opt/mainloop-runtime/bin/launch "/opt/mainloop-runtime/bin/$PROVIDER" \
 && cp -L /bin/bash /usr/bin/git /usr/bin/rg /opt/mainloop-runtime/bin/ \
 && cp -RL --preserve=links /usr/libexec/git-core /opt/mainloop-runtime/libexec/ \
 && cp -RL /usr/share/git-core /opt/mainloop-runtime/share/ \
 && cp -L /lib/*.so* /usr/lib/*.so* /opt/mainloop-runtime/lib/ \
 && find /opt/mainloop-runtime -type f -exec sh -c '\
   for file do \
     case "$file" in /opt/mainloop-runtime/libexec/claude|/opt/mainloop-runtime/libexec/codex) continue ;; esac; \
     if patchelf --print-interpreter "$file" >/dev/null 2>&1; then \
       patchelf --set-interpreter /opt/mainloop-runtime/lib/ld-musl-*.so.1 "$file"; \
       patchelf --set-rpath /opt/mainloop-runtime/lib "$file"; \
     fi; \
   done' sh {} + \
 && chmod -R a+rX /opt/mainloop-runtime \
 && /opt/mainloop-runtime/bin/launch --write-manifest "$PROVIDER"

FROM scratch
COPY --from=payload /opt/mainloop-runtime/ /
# Substrate mounts the image root at /opt/mainloop-runtime, not its image config.
LABEL org.opencontainers.image.description="Kagent native runtime payload (musl-private ABI, schema 1)"
