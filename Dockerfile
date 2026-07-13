# ---- build stage ----
FROM golang:1.23-alpine AS build
WORKDIR /src

# The AST structural fingerprint (internal/astfp) uses go-tree-sitter, which wraps
# the tree-sitter C parser via cgo — so the build needs a C toolchain. build-base
# pulls in gcc + musl-dev.
RUN apk add --no-cache build-base

# Cache deps first.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# cgo is required (tree-sitter); link everything statically against musl so the
# resulting binary carries no dynamic deps and still runs on distroless/static.
RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-s -w -extldflags '-static'" \
    -o /out/dittobench-api ./cmd/dittobench-api

# ---- validator sandbox runtime ----
# This opt-in target runs the API beside a host Docker daemon. The binary shells
# out to both git and docker when it materializes and runs miner submissions.
# Keep the ordinary runtime below as the default target for hosted practice.
FROM docker:28.3.3-cli-alpine3.22 AS sandbox
RUN apk add --no-cache git
COPY --from=build /out/dittobench-api /dittobench-api
ENV HOME=/tmp
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/dittobench-api", "-port", "8000"]

# ---- model-relay ----
# The model-pinning gateway (cmd/model-relay). A GPU-less validator runs it beside
# the scorer to front Chutes: it forces the frozen model and thinking mode and
# keeps the Chutes key out of the sandbox. Pure Go (no cgo), so a static build.
FROM build AS relay-build
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/model-relay ./cmd/model-relay

FROM gcr.io/distroless/static-debian12:nonroot AS relay
COPY --from=relay-build /out/model-relay /model-relay
EXPOSE 11435
ENTRYPOINT ["/model-relay"]

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/dittobench-api /dittobench-api
EXPOSE 8000
ENTRYPOINT ["/dittobench-api", "-port", "8000"]
