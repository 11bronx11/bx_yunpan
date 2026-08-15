# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25.13
FROM golang:${GO_VERSION}-alpine AS build

ARG TARGET=api
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN case "${TARGET}" in api|worker|migrate) ;; *) echo "unsupported TARGET=${TARGET}" >&2; exit 1 ;; esac \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${TARGET}

FROM alpine:3.22

LABEL org.opencontainers.image.title="BX YunPan" \
      org.opencontainers.image.description="Go API, asynchronous worker, and database migration runtime for BX YunPan"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 65532 nonroot \
    && adduser -S -D -H -u 65532 -G nonroot nonroot
WORKDIR /app
ENV TZ=UTC
COPY --from=build --chown=65532:65532 /out/service /app/service
COPY --chown=65532:65532 migrations /app/migrations
USER 65532:65532
EXPOSE 8081
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/service"]
