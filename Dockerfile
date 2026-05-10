# Build stage
FROM golang:1.26 AS builder

WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -a \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o manager ./cmd/manager/

# Runtime stage
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source="https://github.com/SebTardif/kube-rightsize"
LABEL org.opencontainers.image.title="kube-rightsize"
LABEL org.opencontainers.image.description="Kubernetes operator for in-place pod resource right-sizing"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
