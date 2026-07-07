# Bard core image. Core is backend-agnostic: it does no mounts and shells out to
# no storage tools -- every backend operation is proxied to a plugin over a unix
# socket. So the image is just the static binary, no userspace storage packages.
#
# RUNTIME_BASE is the final base and is a build-arg so you can swap your own
# hardened base with no fork. The binary is CGO_ENABLED=0 static, so any minimal
# base works. Default is Chainguard static (hardened, CVE-tracked, nonroot 65532).
# The deploy/chart set a shared fsGroup + node runAsUser:0 so a nonroot core talks
# to the plugin sockets -- see docs/hardened-images.md. To fall back to Google
# distroless (root), or any base:
#   podman build --build-arg RUNTIME_BASE=gcr.io/distroless/static-debian12 -t bard-csi .
ARG RUNTIME_BASE=cgr.dev/chainguard/static
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/bard-csi ./cmd/bard-csi

FROM ${RUNTIME_BASE}
COPY --from=build /out/bard-csi /usr/local/bin/bard-csi
ENTRYPOINT ["/usr/local/bin/bard-csi"]
