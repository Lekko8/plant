# syntax=docker/dockerfile:1

# Этап сборки
FROM golang:1.27-alpine AS builder
WORKDIR /src

# Сначала копируем go.mod/go.sum для использования кэша слоёв
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/plant .

# Этап запуска
FROM alpine:3

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/plant /app/plant

# Непривилегированный пользователь: приложению не нужен root
USER nobody

ENV HTTP_ADDR=:8088
EXPOSE 8088

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8088/ping || exit 1

ENTRYPOINT ["/app/plant"]
