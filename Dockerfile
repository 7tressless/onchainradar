# OCR single static binary, runs any subcommand (default: run).
# CGO is off (pure-Go module), so the binary is fully static.

# build stage
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ocr ./cmd/ocr

# runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
# Run as an unprivileged user: the process holds the gas-only agent key, so it
# should never run as root. A fixed uid keeps file ownership predictable.
RUN adduser -D -u 10001 ocr
WORKDIR /app
COPY --from=build /out/ocr /app/ocr
# Non-secret design-time config + migrations travel with the image. Copy them
# owned by the runtime user so the binary can read config/ and migrations/.
COPY --chown=ocr:ocr config/ /app/config/
COPY --chown=ocr:ocr migrations/ /app/migrations/
# Secrets are injected at runtime via env (compose env_file), never baked in.
USER ocr
ENTRYPOINT ["/app/ocr"]
CMD ["run"]
