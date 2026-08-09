# Base image pinned by digest. A floating tag is a reproducibility bug, and this
# repository's entire premise is reproducibility. Get the digest with:
#   docker buildx imagetools inspect golang:1.26-bookworm
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599

# ----------------------------------------------------------------- build stage
FROM ${GO_IMAGE} AS build
WORKDIR /src

# Dependencies first, so a source change does not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Determinism starts at the build. Trimpath removes local filesystem paths from
# the binary, and CGO is off so the build does not vary with the host toolchain.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/spine ./cmd/spine

# ------------------------------------------------------------------ test stage
# The authoritative test run. `task test:container` builds and runs this target.
# Race detection needs CGO, so it is re-enabled here and only here.
FROM ${GO_IMAGE} AS test
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1
CMD ["go", "test", "-race", "-count=1", "./..."]

# --------------------------------------------------------------- runtime stage
# Distroless: no shell, no package manager, nothing to exploit and nothing to
# drift. A reader with a container runtime and nothing else can run this.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/spine /usr/local/bin/spine
USER nonroot:nonroot

# Log data lives here so a bind mount can survive a container restart, which is
# what the crash-recovery tests need.
VOLUME ["/data"]
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/spine"]
CMD ["--help"]
