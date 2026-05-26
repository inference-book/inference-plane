# Multi-stage build for the iplane binary.
#
# Stage 1: build with the full Go toolchain.
# Stage 2: runtime on a minimal base. distroless/static gives us no
# shell, no package manager, and a tiny attack surface. The binary
# needs only ca-certificates (provided by distroless/static) and the
# trusted system roots.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

# Build with stripped symbols and trimmed paths for reproducibility.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/iplane \
    ./cmd/iplane

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/iplane /app/iplane

EXPOSE 8080
USER nonroot:nonroot
# `iplane serve` is the long-running container command. The same binary
# also offers `iplane load` and `iplane gen-names` subcommands, useful
# when running the image as a one-shot via `docker run ... <subcmd>`.
ENTRYPOINT ["/app/iplane"]
CMD ["serve"]
