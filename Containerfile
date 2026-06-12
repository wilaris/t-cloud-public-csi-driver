FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

# The -X symbol paths come from go.mod, because a path that matches nothing links cleanly and
# stamps nothing.
RUN set -eu; \
    module="$(go list -m)"; \
    CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
            -X ${module}/internal/version.version=${VERSION} \
            -X ${module}/internal/version.commit=${GIT_COMMIT} \
            -X ${module}/internal/version.buildDate=${BUILD_DATE}" \
        -o /out/t-cloud-csi-driver ./cmd/t-cloud-csi-driver

FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

# The node service needs the mount, filesystem detection, block device listing and ext4/XFS
# creation tools in the image, because Talos Linux hosts are immutable and provide none of them.
# The pins track the alpine 3.23 branch; bump them together with the base digest.
RUN apk add --no-cache \
        blkid=2.41.4-r0 \
        e2fsprogs=1.47.3-r0 \
        lsblk=2.41.4-r0 \
        mount=2.41.4-r0 \
        umount=2.41.4-r0 \
        xfsprogs=6.17.0-r0

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
ARG IMAGE_SOURCE=

LABEL org.opencontainers.image.source="${IMAGE_SOURCE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build /out/t-cloud-csi-driver /t-cloud-csi-driver

ENTRYPOINT ["/t-cloud-csi-driver"]
