ARG GO_VERSION=1.27.4

FROM golang:${GO_VERSION} AS build
WORKDIR /src

# The module list alone, so an edit to the source reuses the downloaded modules.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev

# CGO is disabled: the binary is fully static and needs no system libraries.
# The HTML report template is embedded, so the binary is all that is needed.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/ferret ./cmd/ferret

FROM ghcr.io/wntrtech/scratch:v1.0.0-3
# The runner calls HTTPS services, so it needs the CA bundle (copied in case the base image has none).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ferret /ferret

# Mount the test repository here and pass paths relative to it, for example:
#   docker run --rm -v "$PWD":/work -e API_KEY=... ferret run --tests tests/suite.yaml --env-file env/dev.env --output reports
WORKDIR /work
ENTRYPOINT ["/ferret"]
CMD ["--help"]
