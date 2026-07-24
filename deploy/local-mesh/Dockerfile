# syntax=docker/dockerfile:1.7
#
# Runtime-only test-harness image. It does NOT compile anything: it wraps a
# statically-linked gosmokeping binary that build.sh produces on the host.
#
# Why not build in-container like the root Dockerfile? node:22 npm dies
# mid-`npm ci` ("Exit handler never called") in this build environment. The
# host toolchain works, produces a CGO_ENABLED=0 static binary with the UI
# embedded, and that runs unmodified on alpine. build.sh compiles it; this
# image just adds a CA bundle, the raw-socket capability, and a non-root user.
#
# Build context is this directory (deploy/local-mesh), so the binary sitting
# beside this file is the only thing copied in.

FROM alpine:3.20
RUN apk add --no-cache libcap ca-certificates tzdata \
    && addgroup -S gosmokeping \
    && adduser -S -G gosmokeping gosmokeping
COPY gosmokeping /opt/smokeping/gosmokeping
RUN setcap cap_net_raw+ep /opt/smokeping/gosmokeping
USER gosmokeping
EXPOSE 8080
ENTRYPOINT ["/opt/smokeping/gosmokeping"]
CMD ["-config", "/opt/smokeping/config.json"]
