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

FROM golang:1.26-bookworm
ENV GOTOOLCHAIN=auto
# kubectl: the operator's ingest Job runs `kubectl patch configmap <result>` (in-cluster
# SA auth) to report the ingested HEAD back to the operator. Pinned to the cluster minor.
ARG KUBECTL_VERSION=v1.33.0
RUN curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && kubectl version --client
COPY --from=builder /out/tatara-ingest /usr/local/bin/tatara-ingest
ENTRYPOINT ["tatara-ingest"]
