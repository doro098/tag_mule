package openai

import (
        "bufio"
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "log/slog"
        "net/http"
        "os/exec"
        "strings"
        "sync"
        "sync/atomic"
        "time"

        "github.com/tag-mule/tag-mule/pkg/storage"
)

// BatchHandler maneja los endpoints de lotes (Batches):
//   - POST  /v1/batches     → crear lote y disparar procesamiento asíncrono
//   - GET   /v1/batches/{id} → consultar estado del lote
type BatchHandler struct {
        store storage.Store
        cfg   *Config
}

// ---------------------------------------------------------------------------
// Variables package-level para el mecanismo de suspensión de cortesía.
// Estas son compartidas entre todas las goroutines de procesamiento para
// coordinar cuándo suspender la torre Debian.
// ---------------------------------------------------------------------------

var (
        suspendTimer   *time.Timer
        suspendTimerMu sync.Mutex
        activeBatches  int32 // contador atómico de lotes en procesamiento
)

// NewBatchHandler crea un nuevo BatchHandler.
func NewBatchHandler(store storage.Store, cfg *Config) *BatchHandler {
        return &BatchHandler{store: store, cfg: cfg}
}

// ---------------------------------------------------------------------------
// POST /v1/batches
// ---------------------------------------------------------------------------

// CreateBatch recibe la petición de creación de lote, la valida,
// almacena el registro con estado "pending", responde inmediatamente
// y dispara la goroutine de procesamiento en segundo plano.
func (h *BatchHandler) CreateBatch(w http.ResponseWriter, r *http.Request) {
        var req BatchCreateRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                writeError(w, http.StatusBadRequest,
                        "invalid_request_error",
                        fmt.Sprintf("JSON inválido en el body: %v", err))
                return
        }

        // Validar que el archivo de entrada existe
        if _, ok := h.store.GetFile(req.InputFileID); !ok {
                writeError(w, http.StatusBadRequest,
                        "invalid_request_error",
                        fmt.Sprintf("El archivo de entrada '%s' no existe. Subí un archivo primero via POST /v1/files.", req.InputFileID))
                return
        }

        // Valores por defecto
        if req.Endpoint == "" {
                req.Endpoint = "/v1/chat/completions"
        }
        if req.CompletionWindow == "" {
                req.CompletionWindow = "24h"
        }

        batchID := generateID("batch_")
        now := time.Now().UnixMilli()

        record := storage.BatchRecord{
                ID:               batchID,
                Endpoint:         req.Endpoint,
                CompletionWindow: req.CompletionWindow,
                InputFileID:      req.InputFileID,
                Status:           storage.BatchStatusPending,
                CreatedAt:        now,
        }

        h.store.SaveBatch(record)

        resp := batchRecordToResponse(record)
        writeJSON(w, http.StatusOK, resp)

        // Disparar procesamiento en segundo plano.
        // La respuesta ya fue enviada, el cliente no se bloquea.
        slog.Info("lote creado, iniciando procesamiento en background",
                "batch_id", batchID,
                "input_file_id", req.InputFileID,
        )
        go h.processBatch(batchID)
}

// ---------------------------------------------------------------------------
// GET /v1/batches/{id}
// ---------------------------------------------------------------------------

// GetBatch retorna el estado actual del lote solicitado.
// El cliente puede hacer polling a este endpoint hasta que
// el estado sea "completed" o "failed".
func (h *BatchHandler) GetBatch(w http.ResponseWriter, r *http.Request) {
        id := r.PathValue("id")

        record, ok := h.store.GetBatch(id)
        if !ok {
                writeError(w, http.StatusNotFound,
                        "batch_not_found",
                        fmt.Sprintf("El lote '%s' no existe.", id))
                return
        }

        resp := batchRecordToResponse(record)
        writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// batchRecordToResponse convierte un BatchRecord del storage a la
// respuesta JSON compatible con la API de OpenAI.
// ---------------------------------------------------------------------------

func batchRecordToResponse(b storage.BatchRecord) BatchCreateResponse {
        resp := BatchCreateResponse{
                ID:               b.ID,
                Object:           "batch",
                Endpoint:         b.Endpoint,
                InputFileID:      b.InputFileID,
                CompletionWindow: b.CompletionWindow,
                Status:           string(b.Status),
                CreatedAt:        b.CreatedAt,
                InProgressAt:     b.InProgressAt,
                CompletedAt:      b.CompletedAt,
                FailedAt:         b.FailedAt,
                OutputFileID:     b.OutputFileID,
                ErrorFileID:      b.ErrorFileID,
        }

        // Agregar contadores de solicitudes solo si están disponibles
        if b.TotalRequests > 0 {
                resp.RequestCounts = &BatchRequestCounts{
                        Total:     b.TotalRequests,
                        Completed: b.CompletedRequests,
                        Failed:    b.FailedRequests,
                }
        }

        // Agregar información de error si el lote falló
        if b.Status == storage.BatchStatusFailed && b.Error != "" {
                resp.Error = &BatchError{
                        Code:    "batch_failed",
                        Message: b.Error,
                }
        }

        return resp
}

// ---------------------------------------------------------------------------
// processBatch - Goroutine principal de procesamiento
// ---------------------------------------------------------------------------

// processBatch ejecuta toda la lógica de procesamiento de un lote:
//  1. Verifica si se usa proveedor externo o hardware local.
//  2. Si es local, despierta la torre vía WoL si es necesario.
//  3. Procesa cada línea del JSONL enviando peticiones al LLM.
//  4. Genera el archivo de salida y actualiza el estado del lote.
//  5. Programa la suspensión de la torre tras un tiempo de cortesía.
func (h *BatchHandler) processBatch(batchID string) {
        // Marcar como "processing" inmediatamente
        now := time.Now().UnixMilli()
        h.store.UpdateBatch(batchID, func(b *storage.BatchRecord) {
                b.Status = storage.BatchStatusProcessing
                b.InProgressAt = now
        })

        // Incrementar contador de lotes activos y cancelar cualquier suspensión pendiente
        atomic.AddInt32(&activeBatches, 1)
        cancelPendingSuspend()

        slog.Info("lote en procesamiento", "batch_id", batchID)

        // Obtener el registro actualizado
        record, ok := h.store.GetBatch(batchID)
        if !ok {
                slog.Error("lote desapareció del storage", "batch_id", batchID)
                h.decrementAndMaybeSchedule()
                return
        }

        // Leer archivo de entrada
        inputRecord, ok := h.store.GetFile(record.InputFileID)
        if !ok {
                h.failBatch(batchID, "el archivo de entrada asociado no fue encontrado en el storage")
                h.decrementAndMaybeSchedule()
                return
        }

        // Determinar el endpoint del proveedor y la API key
        var providerURL string
        var apiKey string

        if h.cfg.UseExternalProvider {
                // ---- Ruta: Proveedor Externo ----
                baseURL := strings.TrimRight(h.cfg.ExternalAPIURL, "/")
                // Si el usuario ya incluye /v1, no lo duplicamos
                if strings.HasSuffix(baseURL, "/v1") {
                        providerURL = baseURL + "/chat/completions"
                } else {
                        providerURL = baseURL + "/v1/chat/completions"
                }
                apiKey = h.cfg.ExternalAPIKey
                slog.Info("usando proveedor externo", "url", providerURL)
        } else {
                // ---- Ruta: Hardware Local (Ollama en la torre) ----
                ollamaBaseURL := fmt.Sprintf("http://%s:%s", h.cfg.TorreIP, h.cfg.OllamaPort)
                ollamaChatURL := ollamaBaseURL + "/v1/chat/completions"

                // Verificar si Ollama ya está respondiendo
                if !h.isOllamaReachable(ollamaBaseURL) {
                        slog.Info("torre dormida o Ollama no responde, intentando Wake-on-LAN",
                                "torre_ip", h.cfg.TorreIP,
                                "mac", h.cfg.TorreMAC,
                        )

                        if err := h.wakeAndPoll(ollamaBaseURL); err != nil {
                                h.failBatch(batchID, fmt.Sprintf("imposible despertar la torre: %v", err))
                                h.decrementAndMaybeSchedule()
                                return
                        }
                } else {
                        slog.Info("Ollama ya está en línea", "url", ollamaBaseURL)
                }

                providerURL = ollamaChatURL
                apiKey = "" // Ollama local no requiere API key
        }

        // Procesar las líneas del archivo JSONL de entrada
        outputLines, total, completed, failed := h.processJSONLines(inputRecord.Bytes, providerURL, apiKey)

        // Construir y guardar el archivo de salida
        outputData := []byte(strings.Join(outputLines, "\n"))
        if len(outputData) > 0 {
                outputData = append(outputData, '\n') // JSONL siempre termina con newline
        }

        outputID := generateID("file-")
        completedAt := time.Now().UnixMilli()

        outputRecord := storage.FileRecord{
                ID:        outputID,
                Filename:  fmt.Sprintf("%s_output.jsonl", batchID),
                Bytes:     outputData,
                Purpose:   "batch_output",
                CreatedAt: completedAt,
        }
        h.store.SaveFile(outputRecord)

        // Actualizar el lote a "completed"
        h.store.UpdateBatch(batchID, func(b *storage.BatchRecord) {
                b.Status = storage.BatchStatusCompleted
                b.OutputFileID = outputID
                b.CompletedAt = completedAt
                b.TotalRequests = total
                b.CompletedRequests = completed
                b.FailedRequests = failed
        })

        slog.Info("lote completado exitosamente",
                "batch_id", batchID,
                "total", total,
                "completed", completed,
                "failed", failed,
                "output_file_id", outputID,
        )

        h.decrementAndMaybeSchedule()
}

// ---------------------------------------------------------------------------
// processJSONLines lee el contenido JSONL línea por línea, envía cada
// petición al proveedor y construye las líneas de salida.
// ---------------------------------------------------------------------------
func (h *BatchHandler) processJSONLines(inputData []byte, providerURL, apiKey string) (outputLines []string, total, completed, failed int) {
        scanner := bufio.NewScanner(bytes.NewReader(inputData))
        // Buffer generoso para líneas largas (documentos grandes con entidades)
        scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

        for scanner.Scan() {
                line := strings.TrimSpace(scanner.Text())
                if line == "" {
                        continue
                }

                // Estructura de entrada compatible con OpenAI Batch API
                var inputLine struct {
                        CustomID string          `json:"custom_id"`
                        Method   string          `json:"method"`
                        URL      string          `json:"url"`
                        Body     json.RawMessage `json:"body"`
                }

                if err := json.Unmarshal([]byte(line), &inputLine); err != nil {
                        // JSON inválido: registrar error y continuar con la siguiente línea
                        failed++
                        total++
                        outputLines = append(outputLines, buildErrorOutputLine(
                                inputLine.CustomID,
                                "invalid_json",
                                fmt.Sprintf("Error parseando JSON de la línea: %v", err),
                        ))
                        slog.Warn("línea JSON inválida en lote", "custom_id", inputLine.CustomID, "error", err)
                        continue
                }

                total++

                // Enviar la petición de chat completion al proveedor
                respBody, statusCode, err := h.sendChatCompletion(providerURL, apiKey, inputLine.Body)
                if err != nil {
                        failed++
                        outputLines = append(outputLines, buildErrorOutputLine(
                                inputLine.CustomID,
                                "provider_error",
                                fmt.Sprintf("Error del proveedor: %v", err),
                        ))
                        slog.Warn("error enviando petición al proveedor",
                                "custom_id", inputLine.CustomID,
                                "error", err,
                        )
                        continue
                }

                // Éxito: construir la línea de salida
                completed++
                reqID := generateID("req_")
                outputLines = append(outputLines, buildSuccessOutputLine(
                        inputLine.CustomID,
                        respBody,
                        statusCode,
                        reqID,
                ))
        }

        if err := scanner.Err(); err != nil {
                slog.Error("error escaneando archivo JSONL de entrada", "error", err)
        }

        return outputLines, total, completed, failed
}

// ---------------------------------------------------------------------------
// sendChatCompletion envía una petición HTTP POST al endpoint de chat
// completions del proveedor (Ollama o externo) y retorna el body
// de la respuesta, el status code y cualquier error.
// ---------------------------------------------------------------------------
func (h *BatchHandler) sendChatCompletion(providerURL, apiKey string, body json.RawMessage) (responseBody []byte, statusCode int, err error) {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
        defer cancel()

        httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, providerURL, bytes.NewReader(body))
        if err != nil {
                return nil, 0, fmt.Errorf("creando petición HTTP: %w", err)
        }

        httpReq.Header.Set("Content-Type", "application/json")
        if apiKey != "" {
                httpReq.Header.Set("Authorization", "Bearer "+apiKey)
        }

        resp, err := http.DefaultClient.Do(httpReq)
        if err != nil {
                return nil, 0, fmt.Errorf("petición HTTP fallida: %w", err)
        }
        defer resp.Body.Close()

        responseBody, err = io.ReadAll(resp.Body)
        if err != nil {
                return nil, resp.StatusCode, fmt.Errorf("leyendo respuesta HTTP: %w", err)
        }

        // Si el status code no es 2xx, retornar como error
        if resp.StatusCode < 200 || resp.StatusCode >= 300 {
                return responseBody, resp.StatusCode, fmt.Errorf("proveedor retornó status %d: %s", resp.StatusCode, truncateStr(string(responseBody), 200))
        }

        return responseBody, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// Constructores de líneas de salida JSONL
// ---------------------------------------------------------------------------

// buildSuccessOutputLine construye una línea de salida exitosa.
// Parsea el body de respuesta del proveedor para extraer los campos
// del ChatCompletion y los envuelve en el formato BatchOutputLine.
func buildSuccessOutputLine(customID string, rawRespBody []byte, statusCode int, reqID string) string {
        // Intentar parsear como ChatCompletionResponse
        var chatResp ChatCompletionResponse
        if err := json.Unmarshal(rawRespBody, &chatResp); err != nil {
                // Si no se puede parsear, usar el body crudo como string en content
                chatResp = ChatCompletionResponse{
                        ID:      reqID,
                        Object:  "chat.completion",
                        Created: time.Now().Unix(),
                        Model:   "unknown",
                        Choices: []ChatChoice{{
                                Index: 0,
                                Message: ChatMessage{
                                        Role:    "assistant",
                                        Content: string(rawRespBody),
                                },
                                FinishReason: "stop",
                        }},
                }
        }

        output := BatchOutputLine{
                ID:       generateID("batch_req_"),
                CustomID: customID,
                Response: BatchOutputResponse{
                        StatusCode: statusCode,
                        RequestID:  reqID,
                        Body:       chatResp,
                },
        }

        b, _ := json.Marshal(output)
        return string(b)
}

// buildErrorOutputLine construye una línea de salida con error.
func buildErrorOutputLine(customID, code, message string) string {
        output := BatchOutputLine{
                ID:       generateID("batch_req_"),
                CustomID: customID,
                Response: BatchOutputResponse{
                        StatusCode: 0,
                        RequestID:  "",
                },
                Error: &BatchOutputError{
                        Code:    code,
                        Message: message,
                },
        }

        b, _ := json.Marshal(output)
        return string(b)
}

// ---------------------------------------------------------------------------
// Wake-on-LAN y verificación de Ollama
// ---------------------------------------------------------------------------

// isOllamaReachable hace un HTTP GET rápido a /api/tags de Ollama
// para verificar que el servicio está respondiendo.
func (h *BatchHandler) isOllamaReachable(ollamaBaseURL string) bool {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaBaseURL+"/api/tags", nil)
        if err != nil {
                return false
        }

        resp, err := http.DefaultClient.Do(req)
        if err != nil {
                return false
        }
        defer resp.Body.Close()

        return resp.StatusCode == http.StatusOK
}

// wakeAndPoll ejecuta el comando wakeonlan con la MAC configurada
// y luego hace polling cada WAKE_POLL_INTERVAL segundos hasta que
// Ollama responda o se alcance el WAKE_TIMEOUT.
//
// El contexto con timeout garantiza que nunca bloquearemos
// indefinidamente esperando la torre.
func (h *BatchHandler) wakeAndPoll(ollamaBaseURL string) error {
        // Ejecutar wakeonlan
        slog.Info("enviando paquete mágico Wake-on-LAN", "mac", h.cfg.TorreMAC)
        cmd := exec.Command("wakeonlan", h.cfg.TorreMAC)
        if output, err := cmd.CombinedOutput(); err != nil {
                return fmt.Errorf("wakeonlan falló: %w, salida: %s", err, string(output))
        }

        // Polling con timeout
        ctx, cancel := context.WithTimeout(context.Background(), h.cfg.WakeTimeout)
        defer cancel()

        ticker := time.NewTicker(h.cfg.WakePollInterval)
        defer ticker.Stop()

        slog.Info("esperando que la torre despierte y Ollama esté en línea...",
                "max_wait", h.cfg.WakeTimeout,
                "poll_interval", h.cfg.WakePollInterval,
        )

        for {
                select {
                case <-ctx.Done():
                        return fmt.Errorf("timeout: la torre no respondió después de %v", h.cfg.WakeTimeout)

                case <-ticker.C:
                        if h.isOllamaReachable(ollamaBaseURL) {
                                slog.Info("Ollama está en línea, la torre ha despertado correctamente",
                                        "ollama_url", ollamaBaseURL,
                                )
                                // Esperar unos segundos extra para que el modelo de Ollama
                                // esté completamente cargado en memoria (especialmente en
                                // hardware limitado con 8GB RAM)
                                slog.Info("esperando que el modelo esté listo...", "grace_period", "5s")
                                time.Sleep(5 * time.Second)
                                return nil
                        }
                        slog.Debug("Ollama aún no responde, reintentando...",
                                "elapsed", time.Since(time.Now().Add(-h.cfg.WakeTimeout)),
                        )
                }
        }
}

// ---------------------------------------------------------------------------
// Mecanismo de suspensión de cortesía de la torre
// ---------------------------------------------------------------------------

// cancelPendingSuspend cancela cualquier timer de suspensión pendiente.
// Se llama cuando un nuevo lote comienza a procesarse, para evitar
// que la torre se suspenda mientras hay trabajo activo.
func cancelPendingSuspend() {
        suspendTimerMu.Lock()
        defer suspendTimerMu.Unlock()
        if suspendTimer != nil {
                suspendTimer.Stop()
                suspendTimer = nil
                slog.Debug("suspensión pendiente cancelada (nuevo lote en proceso)")
        }
}

// scheduleSuspend programa la suspensión de la torre después de
// SUSPEND_DELAY. Si otro lote comienza antes de que el timer dispare,
// cancelPendingSuspend() lo cancelará.
func scheduleSuspend(cfg *Config) {
        suspendTimerMu.Lock()
        defer suspendTimerMu.Unlock()

        // Detener timer previo si existe
        if suspendTimer != nil {
                suspendTimer.Stop()
        }

        suspendTimer = time.AfterFunc(cfg.SuspendDelay, func() {
                suspendTower(cfg)
        })

        slog.Info("suspensión de torre programada",
                "delay", cfg.SuspendDelay,
                "ssh_user", cfg.SSHUser,
                "torre_ip", cfg.TorreIP,
        )
}

// decrementAndMaybeSchedule decrementa el contador de lotes activos.
// Si llega a cero y no se usa proveedor externo, programa la
// suspensión de la torre después del tiempo de cortesía.
func (h *BatchHandler) decrementAndMaybeSchedule() {
        if atomic.AddInt32(&activeBatches, -1) == 0 {
                if !h.cfg.UseExternalProvider {
                        scheduleSuspend(h.cfg)
                }
        }
}

// suspendTower ejecuta el comando SSH para suspender la torre Debian.
// Utiliza BatchMode=yes para evitar prompts interactivos y
// StrictHostKeyChecking=no para no requerir confirmación manual
// de la huella del host (se asume red confiable de LAN).
func suspendTower(cfg *Config) {
        slog.Info("suspendiendo torre Debian vía SSH",
                "ip", cfg.TorreIP,
                "user", cfg.SSHUser,
        )

        cmd := exec.Command(
                "ssh",
                "-o", "StrictHostKeyChecking=no",
                "-o", "ConnectTimeout=10",
                "-o", "BatchMode=yes",
                "-o", "ServerAliveInterval=5",
                fmt.Sprintf("%s@%s", cfg.SSHUser, cfg.TorreIP),
                "sudo systemctl suspend",
        )

        if output, err := cmd.CombinedOutput(); err != nil {
                slog.Error("error al suspender la torre",
                        "error", err,
                        "output", string(output),
                )
                // No es fatal: la torre eventualmente se suspenderá por su
                // propia configuración de power management, o se reintentará
                // en el próximo ciclo.
        } else {
                slog.Info("torre Debian suspendida correctamente")
        }
}

// ---------------------------------------------------------------------------
// failBatch marca un lote como fallido y registra el error.
// ---------------------------------------------------------------------------
func (h *BatchHandler) failBatch(batchID, errMsg string) {
        now := time.Now().UnixMilli()
        h.store.UpdateBatch(batchID, func(b *storage.BatchRecord) {
                b.Status = storage.BatchStatusFailed
                b.Error = errMsg
                b.FailedAt = now
                b.CompletedAt = now
        })
        slog.Error("lote fallido",
                "batch_id", batchID,
                "error", errMsg,
        )
}

// ---------------------------------------------------------------------------
// Utilidades
// ---------------------------------------------------------------------------

// truncateStr trunca un string a maxLen caracteres y agrega "..." si fue truncado.
func truncateStr(s string, maxLen int) string {
        if len(s) <= maxLen {
                return s
        }
        return s[:maxLen] + "..."
}