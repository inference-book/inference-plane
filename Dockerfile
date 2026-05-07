# Multi-stage build for the controlplane binary.
#
# Stage 1: build with the full Go toolchain.
# Stage 2: runtime on a minimal base. We use distroless/static so the
# image has no shell, no package manager, and a tiny attack surface.
# The control plane binary needs only ca-certificates (provided by
# distroless/static) and the trusted system roots.

FROM golang:1.26.2-alpine AS build
WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

# Build with stripped symbols and trimmed paths for reproducibility.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/controlplane \
    ./cmd/controlplane

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/controlplane /app/controlplane

# Default config path; docker-compose mounts the actual file.
ENV CP_CONFIG_PATH=/etc/controlplane/config.yaml

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/controlplane"]
