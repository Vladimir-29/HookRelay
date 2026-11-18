# syntax=docker/dockerfile:1

# --- Build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src

# Сначала зависимости — кэшируется между сборками.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Статические бинарники: API/Worker и одноразовый применятеля миграций.
# Миграции встроены в hookrelay-migrate через go:embed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hookrelay ./cmd/hookrelay \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hookrelay-migrate ./cmd/migrate

# --- Runtime stage: минимальный образ без shell и утилит ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hookrelay /out/hookrelay-migrate /usr/local/bin/

# По умолчанию — API; APP_MODE=worker и /hookrelay-migrate
# задаются через command/entrypoint в compose.
ENTRYPOINT ["/usr/local/bin/hookrelay"]
