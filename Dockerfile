# ============================================================
# tag-mule - Multi-stage Docker Build
# ============================================================

# --- Etapa 1: Compilación ---
FROM golang:1.22-alpine AS builder

# Dependencias necesarias para la compilación (CGO deshabilitado)
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Copiar go.mod primero para aprovechar la cache de Docker
COPY go.mod ./
RUN go mod download

# Copiar el resto del código fuente
COPY . .

# Compilar binario estático
# CGO_ENABLED=0 asegura un binario independiente sin dependencias C
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /tag-mule ./cmd/main.go

# --- Etapa 2: Imagen final ---
FROM alpine:latest

# Instalar dependencias de sistema necesarias en runtime:
# - wakeonlan: para enviar paquetes mágicos WoL a la torre
# - openssh-client: para ejecutar SSH suspend en la torre
# - ca-certificates: para conexiones HTTPS a proveedores externos
# - tzdata: zona horaria para timestamps correctos
RUN apk add --no-cache \
    wakeonlan \
    openssh-client \
    ca-certificates \
    tzdata

# Crear usuario no-root para seguridad
RUN adduser -D -h /app -s /sbin/nologin appuser

WORKDIR /app

# Copiar el binario compilado desde la etapa anterior
COPY --from=builder /tag-mule /app/tag-mule

# Crear directorio para el archivo .env (se monta como volumen)
RUN mkdir -p /app/data && chown -R appuser:appuser /app

# Cambiar al usuario no-root
USER appuser

# Exponer el puerto del proxy (el valor real se configura vía .env)
EXPOSE 8080

# El archivo .env se monta como volumen en docker-compose.yml
# Datos de archivos JSONL se almacenan en /app/data/
VOLUME ["/app/data"]

ENTRYPOINT ["/app/tag-mule"]