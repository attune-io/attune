# syntax=docker/dockerfile:1

# Build stage: use BUILDPLATFORM so Go runs natively even for cross-arch builds.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:313faae491b410a35402c05d35e7518ae99103d957308e940e1ae2cfa0aac29b AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build with cache mounts for iterative speed.
# Cross-compile via GOOS/GOARCH instead of running the entire compiler under
# QEMU emulation (orders of magnitude faster for linux/arm64 builds).
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o manager ./cmd/manager/

# Runtime stage
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240

LABEL org.opencontainers.image.source="https://github.com/SebTardifLabs/kube-rightsize"
LABEL org.opencontainers.image.title="kube-rightsize"
LABEL org.opencontainers.image.description="Kubernetes operator for in-place pod resource right-sizing"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
