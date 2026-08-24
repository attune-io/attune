# Build stage: use BUILDPLATFORM so Go runs natively even for cross-arch builds.
# BuildKit features (--platform, --mount) are supported natively since Docker 20.10.
# No # syntax directive needed; it triggers a registry pull that fails on macOS
# self-hosted runners where the keychain is locked in headless sessions.
FROM --platform=$BUILDPLATFORM golang:1.27.0@sha256:65b6f280bf050ec5af12716857e8ea8439d694dbba8f31ceeb7630670071f2bb AS builder

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
COPY pkg/ pkg/

# Build with cache mounts for iterative speed.
# Cross-compile via GOOS/GOARCH instead of running the entire compiler under
# QEMU emulation (orders of magnitude faster for linux/arm64 builds).
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOFIPS140=latest GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o manager ./cmd/manager/

# Runtime stage
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

LABEL org.opencontainers.image.source="https://github.com/attune-io/attune"
LABEL org.opencontainers.image.title="attune"
LABEL org.opencontainers.image.description="Kubernetes operator for in-place pod resource right-sizing"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
