FROM golang:1.25-bookworm AS builder
# The embedded DuckDB engine (github.com/duckdb/duckdb-go) links a prebuilt
# static bundle through cgo. Its Linux bundle targets glibc, so Alpine/musl is
# not a supported build base.
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /out/agentray ./cmd/server

FROM debian:bookworm-slim
# libstdc++ is the DuckDB static bundle's runtime C++ dependency; wget is the
# healthcheck client the blue-green deploy's `wget --spider` probe needs —
# bookworm-slim ships neither. ca-certificates is the trust store every
# outbound HTTPS call needs (LLM providers, OAuth token endpoints, webhooks):
# without it the run path dies with "x509: certificate signed by unknown
# authority" against any provider the workspace configures.
RUN apt-get update \
 && apt-get install -y --no-install-recommends libstdc++6 wget ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && adduser --system --no-create-home --group lohi \
 && mkdir -p /data && chown lohi:lohi /data
USER lohi
# DuckDB is a single-writer embedded database; /data is a mounted volume in
# every deployed environment so the file survives container recreation.
ENV DUCKDB_PATH=/data/agentray.duckdb
COPY --from=builder /out/agentray /usr/local/bin/agentray
EXPOSE 8080
ENTRYPOINT ["agentray"]
