# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
# The build stage runs on the builder's own platform and cross-compiles, so a
# multi-arch image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

# Cache modules first so code changes don't re-download the world.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# Static, stripped binary. pgx is pure Go and the UI is go:embed'ed, so the
# result is one self-contained file on a distroless base.
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/billing ./cmd/billing

# ---- runtime --------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/billing /usr/local/bin/billing

USER nonroot:nonroot
EXPOSE 8080

# `serve` is the default; override with `worker`, `migrate`, `tenants …`, `keys …`.
ENTRYPOINT ["/usr/local/bin/billing"]
CMD ["serve"]
