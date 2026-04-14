# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

ENV GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/apod-server .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl && \
    addgroup -S app && adduser -S app -G app

WORKDIR /app
ENV IMAGE_CACHE_DIR=/app/cache/images

COPY --from=builder /out/apod-server /app/apod-server
RUN mkdir -p /app/cache/images && chown -R app:app /app

USER app
EXPOSE 8080

ENTRYPOINT ["/app/apod-server"]
