FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/agentray ./cmd/server

FROM alpine:3.20
RUN adduser -D -H lohi
USER lohi
COPY --from=builder /out/agentray /usr/local/bin/agentray
EXPOSE 8080
ENTRYPOINT ["agentray"]
