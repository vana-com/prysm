# Container image for Vana's Prysm beacon node.
#
# Built by .github/workflows/vana-release.yml for linux/amd64 and linux/arm64, and usable by hand:
#
#   docker build -f vana/docker/Dockerfile \
#     --build-arg TAG="$(git describe --tags)" --build-arg COMMIT="$(git rev-parse HEAD)" \
#     -t prysm-beacon:local .
#
# The build stage runs on the target platform (under emulation for a foreign one), so blst compiles
# natively and no cross toolchain is needed here. The runtime base is debian-slim rather than
# distroless so a node can still be shelled into, which has repeatedly mattered on the devnet, and
# it matches the builder's glibc.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS build

# Version metadata, mirroring the x_defs in runtime/version/BUILD.bazel. Without these the binary
# reports "Unknown" and "Moments ago", leaving a deployed image impossible to identify. The import
# path below is the Go module path from go.mod, which a fork does not change; -X against a symbol
# that does not exist is silently ignored, so a wrong path here fails quietly.
ARG TAG=dev
ARG COMMIT=unknown

WORKDIR /src

# Copy the module graph first so dependency download caches separately from source edits.
# third_party comes too: go.mod replaces github.com/tyler-smith/go-bip39 with a local directory,
# and go mod download reads that directory's go.mod.
COPY go.mod go.sum ./
COPY third_party/ third_party/
RUN go mod download

COPY . .

RUN set -eux; \
    V=github.com/OffchainLabs/prysm/v7/runtime/version; \
    DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
    DATE_UNIX="$(date -u +%s)"; \
    CGO_ENABLED=1 go build -trimpath \
      -ldflags "-s -w -X ${V}.gitTag=${TAG} -X ${V}.gitCommit=${COMMIT} -X ${V}.buildDate=${DATE} -X ${V}.buildDateUnix=${DATE_UNIX}" \
      -o /out/beacon-chain ./cmd/beacon-chain

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/beacon-chain /usr/local/bin/beacon-chain
ENTRYPOINT ["/usr/local/bin/beacon-chain"]
