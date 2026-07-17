# ============================================================
# tag-mule - Multi-stage Docker Build
# ============================================================
# modernc.org/sqlite es Go puro, no necesita CGO.
# ============================================================

# --- Etapa 1: Compilación ---
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Cache de dependencias
# Solo go.mod (go.sum se genera automaticamente con go mod download)
COPY go.mod ./
RUN go mod download

# Código fuente
COPY . .

# Compilar binario estático (sin CGO porque modernc.org/sqlite es Go puro)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /tag-mule ./cmd/main.go

# --- Etapa 2: Imagen final ---
FROM alpine:latest

# Opcionales: solo se usan si TORRE_MAC está configurada
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    wakeonlan \
    openssh-client

RUN adduser -D -h /app -s /sbin/nologin appuser

WORKDIR /app

COPY --from=builder /tag-mule /app/tag-mule

# Directorio para la base de datos SQLite
RUN mkdir -p /app/data && chown -R appuser:appuser /app

USER appuser

EXPOSE 8080

VOLUME ["/app/data"]

ENTRYPOINT ["/app/tag-mule"]
