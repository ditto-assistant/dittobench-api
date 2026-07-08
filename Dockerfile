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

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dittobench-api /dittobench-api
EXPOSE 8000
ENTRYPOINT ["/dittobench-api", "-port", "8000"]
