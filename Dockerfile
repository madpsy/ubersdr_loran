# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_loran
# ---------------------------------------------------------------------------
FROM golang:1.22-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_loran .

# ---------------------------------------------------------------------------
# Stage 2: runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        wget \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false loran

# Copy binary
COPY --from=go-builder /out/ubersdr_loran /usr/local/bin/ubersdr_loran

# Copy static web files
COPY static/ /usr/local/share/ubersdr_loran/static/

# Copy entrypoint
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER loran

# Expose the scope web UI port (default 6088; override with WEB_PORT env var)
EXPOSE 6088

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/bin/wget", "-q", "-O", "/dev/null", "http://localhost:6088/"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
