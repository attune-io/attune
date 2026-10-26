# Build stage: use BUILDPLATFORM so Go runs natively even for cross-arch builds.
# BuildKit features (--platform, --mount) are supported natively since Docker 20.10.
# No # syntax directive needed; it triggers a registry pull that fails on macOS
# self-hosted runners where the keychain is locked in headless sessions.
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

LABEL org.opencontainers.image.source="https://github.com/attune-io/attune"
LABEL org.opencontainers.image.title="attune"
LABEL org.opencontainers.image.description="Kubernetes operator for in-place pod resource right-sizing"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
