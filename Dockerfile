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
# libstdc++ is the DuckDB static bundle's runtime C++ dependency.
RUN apt-get update \
 && apt-get install -y --no-install-recommends libstdc++6 \
 && rm -rf /var/lib/apt/lists/* \
 && adduser --system --no-create-home --group lohi
USER lohi
COPY --from=builder /out/agentray /usr/local/bin/agentray
EXPOSE 8080
ENTRYPOINT ["agentray"]
