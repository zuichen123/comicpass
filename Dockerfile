# syntax=docker/dockerfile:1

FROM golang:1.23-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY configs ./configs

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/napcat-jm-go ./cmd/napcat-jm-go

FROM debian:12-slim
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates \
       tzdata \
       chromium \
       fonts-noto-cjk \
       zip \
       openssh-client \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 10001 appuser

COPY --from=builder /out/napcat-jm-go /app/bin/napcat-jm-go
COPY configs /app/configs

RUN mkdir -p /app/pdf /app/manga /app/cbz /app/logs \
    && chown -R appuser:appuser /app

USER appuser

EXPOSE 8071 18000

CMD ["/app/bin/napcat-jm-go"]
