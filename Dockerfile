# Build stage: produce a fully static binary (no CGO, no libc dependency)
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.5-alpine3.24 AS builder

WORKDIR /app

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=vendor -trimpath -tags netgo,osusergo \
    -ldflags="-s -w -extldflags '-static' -X 'wiki-go/internal/version.Version=${VERSION}'" \
    -o wiki-go .

# Empty data directory so an anonymous volume is writable by the runtime user
RUN mkdir -p /out/wiki/data

# Final stage: empty image containing only the binary
FROM scratch

ARG PUID=1000
ARG PGID=1000

WORKDIR /wiki

COPY --from=builder --chown=${PUID}:${PGID} /out/wiki/data /wiki/data
COPY --from=builder --chmod=755 /app/wiki-go /wiki/wiki-go

USER ${PUID}:${PGID}

EXPOSE 8080 443

VOLUME ["/wiki/data"]

ENTRYPOINT ["/wiki/wiki-go"]

CMD []
