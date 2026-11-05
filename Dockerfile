# Web application: a static binary in an empty image, with the agent binaries
# it distributes.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/server ./cmd/server
RUN mkdir -p /out/dl /out/data && \
    for t in amd64: arm64: arm:7; do \
      arch=${t%:*}; arm=${t#*:}; name=$arch; [ "$arch" = arm ] && name=armv7; \
      GOOS=linux GOARCH=$arch GOARM=$arm go build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" \
        -o /out/dl/uptime-battle-agent-linux-$name ./cmd/agent; \
    done && \
    cd /out/dl && sha256sum uptime-battle-agent-* > SHA256SUMS

FROM scratch
COPY --from=build /out/server /server
COPY --from=build /out/dl /srv/dl
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/server", "healthcheck"]
ENTRYPOINT ["/server"]
