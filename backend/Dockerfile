ARG DOCKER_REGISTRY=docker.io
ARG GO_VERSION=1.25.13
FROM ${DOCKER_REGISTRY}/library/golang:${GO_VERSION}-alpine AS build

ARG GO_PROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GO_PROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG GO_BUILD_PARALLELISM=1
ARG GO_BUILD_MAX_PROCS=2
ARG TARGET=api
RUN case "${TARGET}" in api|worker|aisvc|migrate) ;; *) echo "unsupported TARGET=${TARGET}" >&2; exit 1 ;; esac \
    && CGO_ENABLED=0 GOMAXPROCS="${GO_BUILD_MAX_PROCS}" \
       go build -p="${GO_BUILD_PARALLELISM}" -trimpath -ldflags="-s -w" -o /out/service ./cmd/${TARGET}

FROM ${DOCKER_REGISTRY}/library/alpine:3.22

LABEL org.opencontainers.image.title="BX YunPan" \
      org.opencontainers.image.description="Go API, asynchronous worker, AI gRPC service, and database migration runtime for BX YunPan"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 65532 nonroot \
    && adduser -S -D -H -u 65532 -G nonroot nonroot
WORKDIR /app
ENV TZ=UTC
COPY --from=build --chown=65532:65532 /out/service /app/service
COPY --chown=65532:65532 migrations /app/migrations
USER 65532:65532
EXPOSE 8081 8082
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/service"]
