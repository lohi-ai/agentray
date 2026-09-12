FROM golang:1.25-alpine AS builder
# The embedded DuckDB engine (github.com/duckdb/duckdb-go) links a prebuilt
# static bundle through cgo, so the build needs a C/C++ toolchain and CGO on.
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o /out/agentray ./cmd/server

FROM alpine:3.20
# libstdc++ is the DuckDB static bundle's only runtime dependency.
RUN apk add --no-cache libstdc++ && adduser -D -H lohi
USER lohi
COPY --from=builder /out/agentray /usr/local/bin/agentray
EXPOSE 8080
ENTRYPOINT ["agentray"]
