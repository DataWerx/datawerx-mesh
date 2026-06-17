# syntax=docker/dockerfile:1

# build stage

# Produce a fully static, CGO-free binary so it can run in a distroless (or
# scratch) image with no libc dependency.
FROM golang:1.26 AS build
WORKDIR /workspace

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build.
COPY cmd/ cmd/
COPY pkg/ pkg/
ARG TARGETOS=linux
ARG TARGETARCH=amd64
# VERSION stamps the build identity surfaced in the agent's startup log banner
# (pkg/logging.Version). Pass --build-arg VERSION=<tag> in CI; defaults to "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -X github.com/DataWerx/datawerx-mesh/pkg/logging.Version=${VERSION}" \
    -o /out/dwx-manager ./cmd/manager

# runtime stage 

# The ClusterSetIP NAT, overlap remap, and MeshNetworkPolicy features shell out
# to `iptables`/`ip6tables` (via go-iptables), so the runtime image MUST contain
# those binaries. A pure distroless/static image does not, which makes the agent
# fail at startup ("nat: opening iptables (is the iptables binary present?)").
# We use a small Alpine runtime that ships iptables; the static (CGO_ENABLED=0)
# binary runs on musl unchanged. Alpine's iptables is nft-backed, matching modern
# kernels and kind nodes.
FROM alpine:3.20
RUN apk add --no-cache iptables ip6tables
WORKDIR /
COPY --from=build /out/dwx-manager /dwx-manager

# In-cluster the agent needs NET_ADMIN (and, in WireGuard mode, the wireguard
# kernel module); programming iptables also requires root. Those are granted via
# the DaemonSet securityContext rather than baked into the image. We default to a
# non-root UID for image hygiene; the DaemonSet overrides to root where required.
USER 65532:65532
ENTRYPOINT ["/dwx-manager"]
