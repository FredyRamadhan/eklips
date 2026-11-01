# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/eklips ./cmd/eklips

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    ffmpeg \
    libasound2 \
    pulseaudio-utils \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -r -g 10001 eklips && \
    useradd -r -u 10001 -g eklips -G audio -d /data -s /usr/sbin/nologin eklips && \
    mkdir -p /data /music && \
    chown -R eklips:eklips /data /music

COPY --from=builder /out/eklips /usr/local/bin/eklips

USER eklips:eklips
WORKDIR /data
VOLUME ["/data", "/music"]

ENV EKLIPS_DB=/data/eklips.db \
    EKLIPS_SINK=http \
    EKLIPS_BIND=0.0.0.0 \
    EKLIPS_PORT=8080

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["/usr/local/bin/eklips"]
CMD ["serve", "--sink=http", "--bind=0.0.0.0", "--port=8080", "--db=/data/eklips.db", "--allow-remote"]
