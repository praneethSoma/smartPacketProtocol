# ── Stage 1: Build ──────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
# No go.sum — stdlib-only project
COPY . .

RUN go build -o bin/router  ./cmd/router  && \
    go build -o bin/sender  ./cmd/sender  && \
    go build -o bin/receiver ./cmd/receiver

# ── Stage 2: Runtime ────────────────────────────────────────
FROM alpine:3.19

WORKDIR /app
COPY --from=builder /app/bin/    ./bin/
COPY --from=builder /app/configs/ ./configs/

# No default entrypoint — each service sets its own command.
