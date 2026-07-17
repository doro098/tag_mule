
// tag-mule: Gateway/Proxy de Inteligencia Artificial unificado.
//
// Este microservicio headless en Go implementa compatibilidad con la
// API de OpenAI Batch, actuando como intermediario que puede:
//   - Enviar peticiones a un proveedor externo (Z.ai, OpenAI, etc.)
//   - Despertar una torre Debian vía Wake-on-LAN, procesar con Ollama
//     y suspender la torre automáticamente tras un tiempo de cortesía.
//
// Uso:
//
//	go run ./cmd/main.go
//
// El servidor lee configuración de un archivo .env en el directorio
// actual, o de las variables de entorno del sistema operativo.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tag-mule/tag-mule/pkg/env"
	"github.com/tag-mule/tag-mule/pkg/openai"
	"github.com/tag-mule/tag-mule/pkg/storage"
)

func main() {
	// -----------------------------------------------------------------------
	// 1. Cargar configuración desde .env
	// -----------------------------------------------------------------------
	if err := env.Load(".env"); err != nil {
		slog.Warn("error cargando .env, usando variables de entorno del sistema",
			"error", err,
		)
	}

	cfg := buildConfig()

	// -----------------------------------------------------------------------
	// 2. Validación de configuración esencial
	// -----------------------------------------------------------------------
	if cfg.UseExternalProvider {
		if strings.TrimSpace(cfg.ExternalAPIURL) == "" {
			slog.Error("EXTERNAL_API_URL es obligatorio cuando USE_EXTERNAL_PROVIDER=true")
			os.Exit(1)
		}
		slog.Info("modo: proveedor externo",
			"api_url", cfg.ExternalAPIURL,
		)
	} else {
		if strings.TrimSpace(cfg.TorreIP) == "" {
			slog.Error("TORRE_IP es obligatorio cuando USE_EXTERNAL_PROVIDER=false")
			os.Exit(1)
		}

		wolMsg := "desactivado"
		if strings.TrimSpace(cfg.TorreMAC) != "" {
			wolMsg = cfg.TorreMAC
		}

		slog.Info("modo: Ollama local",
			"ollama_host", cfg.TorreIP,
			"ollama_port", cfg.OllamaPort,
			"wol", wolMsg,
		)
	}

	// -----------------------------------------------------------------------
	// 3. Crear almacenamiento persistente (SQLite)
	// -----------------------------------------------------------------------
	dbPath := cfg.DBPath

	// Asegurar que el directorio existe
	if lastSlash := strings.LastIndex(dbPath, "/"); lastSlash >= 0 {
		dir := dbPath[:lastSlash]
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("no se pudo crear el directorio de datos", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		slog.Error("no se pudo inicializar la base de datos", "path", dbPath, "error", err)
		os.Exit(1)
	}
	defer store.Close()

	slog.Info("almacenamiento SQLite listo", "path", dbPath)

	// -----------------------------------------------------------------------
	// 4. Crear handlers
	// -----------------------------------------------------------------------
	fileHandler := openai.NewFileHandler(store)
	batchHandler := openai.NewBatchHandler(store, cfg)

	// -----------------------------------------------------------------------
	// 5. Configurar rutas HTTP (Go 1.22+ method-based routing)
	// -----------------------------------------------------------------------
	mux := http.NewServeMux()

	// Endpoints de la API de OpenAI Batch
	mux.HandleFunc("POST /v1/files", fileHandler.CreateFile)
	mux.HandleFunc("GET /v1/files/{id}/content", fileHandler.GetFileContent)
	mux.HandleFunc("POST /v1/batches", batchHandler.CreateBatch)
	mux.HandleFunc("GET /v1/batches/{id}", batchHandler.GetBatch)

	// Health check simple para monitoreo
	mux.HandleFunc("GET /health", healthCheckHandler)

	// -----------------------------------------------------------------------
	// 6. Iniciar servidor HTTP
	// -----------------------------------------------------------------------
	addr := ":" + cfg.ProxyPort

	slog.Info("tag-mule iniciando",
		"listen", addr,
		"version", "1.0.0",
		"storage", "sqlite",
		"db_path", dbPath,
	)

	if err := http.ListenAndServe(addr, loggingMiddleware(mux)); err != nil {
		slog.Error("error fatal del servidor HTTP", "error", err)
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------
// buildConfig construye la configuración a partir de variables de entorno
// con valores por defecto sensibles.
// -----------------------------------------------------------------------
func buildConfig() *openai.Config {
	return &openai.Config{
		DBPath:             env.GetOrDefault("DB_PATH", "./data/tag-mule.db"),
		TorreIP:            env.GetOrDefault("TORRE_IP", "192.168.1.100"),
		TorreMAC:           env.GetOrDefault("TORRE_MAC", ""),
		SSHUser:            env.GetOrDefault("SSH_USER", "root"),
		ProxyPort:          env.GetOrDefault("PROXY_PORT", "8080"),
		UseExternalProvider: env.GetBoolOrDefault("USE_EXTERNAL_PROVIDER", false),
		ExternalAPIURL:     env.GetOrDefault("EXTERNAL_API_URL", ""),
		ExternalAPIKey:     env.GetOrDefault("EXTERNAL_API_KEY", ""),
		OllamaPort:         env.GetOrDefault("OLLAMA_PORT", "11434"),
		WakeTimeout:        time.Duration(env.GetIntOrDefault("WAKE_TIMEOUT", 600)) * time.Second,
		SuspendDelay:       time.Duration(env.GetIntOrDefault("SUSPEND_DELAY", 120)) * time.Second,
		WakePollInterval:   time.Duration(env.GetIntOrDefault("WAKE_POLL_INTERVAL", 2)) * time.Second,
		SuspendEnabled:     env.GetBoolOrDefault("SUSPEND_ENABLED", false),
	}
}

// -----------------------------------------------------------------------
// healthCheckHandler responde al endpoint de monitoreo de salud.
// -----------------------------------------------------------------------
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok","service":"tag-mule"}`)
}

// -----------------------------------------------------------------------
// loggingMiddleware agrega logging estructurado a cada petición HTTP.
// -----------------------------------------------------------------------
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrapper para capturar el status code
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(lrw, r)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// loggingResponseWriter envuelve http.ResponseWriter para capturar
// el status code escrito por el handler.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}