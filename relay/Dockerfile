# The relay has no third-party dependencies, so the build needs no network and
# the runtime image needs no distro: a static binary on scratch.
FROM golang:1.25-alpine AS build

ARG VERSION=dev
WORKDIR /src

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/mystrelay \
        ./cmd/mystrelay

RUN CGO_ENABLED=0 go vet ./... && go test ./...

FROM scratch

# The relay terminates exactly one TLS connection of its own: the egress proof
# it makes after each reconnect. Verifying it needs a trust store, and scratch
# has none - without this every provider is rejected as untrustworthy and the
# relay never comes up. Client traffic is unaffected either way; it passes
# through opaquely and is verified end to end by the client.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/mystrelay /mystrelay

# Nothing in the relay needs root, a writable filesystem, or a shell. The
# healthcheck re-executes the binary rather than shelling out. Compose
# overrides this uid so it matches the owner of the mounted identity file.
USER 65534:65534

ENTRYPOINT ["/mystrelay"]
