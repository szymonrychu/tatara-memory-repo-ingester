# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-bookworm AS builder
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev git ca-certificates && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath \
    -ldflags "-s -w \
      -X github.com/szymonrychu/tatara-memory-repo-ingester/internal/version.Version=${VERSION} \
      -X github.com/szymonrychu/tatara-memory-repo-ingester/internal/version.Commit=${COMMIT} \
      -X github.com/szymonrychu/tatara-memory-repo-ingester/internal/version.Date=${DATE}" \
    -o /out/tatara-ingest ./cmd/tatara-ingest

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=builder /out/tatara-ingest /tatara-ingest
USER nonroot:nonroot
ENTRYPOINT ["/tatara-ingest"]
