# ---- build stage ----
FROM golang:1.23-alpine AS build
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/dittobench-api ./cmd/dittobench-api

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dittobench-api /dittobench-api
EXPOSE 8000
ENTRYPOINT ["/dittobench-api", "-port", "8000"]
