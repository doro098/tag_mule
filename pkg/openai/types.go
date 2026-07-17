package openai

import (
	"time"
)

// Config contiene toda la configuración del servicio,
// leída de variables de entorno.
type Config struct {
	TorreIP            string
	TorreMAC           string
	SSHUser            string
	ProxyPort          string
	UseExternalProvider bool
	ExternalAPIURL     string
	ExternalAPIKey     string
	OllamaPort         string
	WakeTimeout        time.Duration
	SuspendDelay       time.Duration
	WakePollInterval   time.Duration
}

// --- Estructuras de la API de OpenAI (compatibilidad) ---

// FileUploadResponse es la respuesta al POST /v1/files.
type FileUploadResponse struct {
	ID        string `json:"id"`
	Object    string `json:"object"` // "file"
	Bytes     int    `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

// BatchCreateRequest es el body del POST /v1/batches.
type BatchCreateRequest struct {
	InputFileID      string `json:"input_file_id"`
	Endpoint         string `json:"endpoint"`          // "/v1/chat/completions"
	CompletionWindow string `json:"completion_window"`  // "24h"
}

// BatchCreateResponse es la respuesta al POST /v1/batches.
type BatchCreateResponse struct {
	ID                string `json:"id"`
	Object            string `json:"object"` // "batch"
	Endpoint          string `json:"endpoint"`
	InputFileID       string `json:"input_file_id"`
	CompletionWindow  string `json:"completion_window"`
	Status            string `json:"status"`
	Error             *BatchError `json:"error,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	InProgressAt      int64  `json:"in_progress_at,omitempty"`
	CompletedAt       int64  `json:"completed_at,omitempty"`
	FailedAt          int64  `json:"failed_at,omitempty"`
	ExpiredAt         int64  `json:"expired_at,omitempty"`
	OutputFileID      string `json:"output_file_id,omitempty"`
	ErrorFileID       string `json:"error_file_id,omitempty"`
	RequestCounts     *BatchRequestCounts `json:"request_counts,omitempty"`
}

// BatchGetResponse es la respuesta al GET /v1/batches/{id}.
// Es idéntica a BatchCreateResponse para mantener compatibilidad.
type BatchGetResponse = BatchCreateResponse

// BatchError representa un error de lote.
type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BatchRequestCounts contiene contadores de solicitudes del lote.
type BatchRequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// BatchInputLine representa una línea del archivo JSONL de entrada.
// Formato compatible con la API de OpenAI Batch.
type BatchInputLine struct {
	CustomID string                   `json:"custom_id"`
	Method   string                   `json:"method"`
	URL      string                   `json:"url"`
	Body     ChatCompletionRequest    `json:"body"`
}

// BatchOutputLine representa una línea del archivo JSONL de salida.
type BatchOutputLine struct {
	ID       string                 `json:"id"`
	CustomID string                 `json:"custom_id"`
	Response BatchOutputResponse    `json:"response"`
	Error    *BatchOutputError      `json:"error,omitempty"`
}

// BatchOutputResponse contiene la respuesta del LLM para una solicitud individual.
type BatchOutputResponse struct {
	StatusCode int                    `json:"status_code"`
	RequestID  string                 `json:"request_id"`
	Body       ChatCompletionResponse `json:"body"`
}

// BatchOutputError representa un error para una solicitud individual del lote.
type BatchOutputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ChatCompletionRequest es el formato de petición de chat completions.
type ChatCompletionRequest struct {
	Model       string            `json:"model"`
	Messages    []ChatMessage     `json:"messages"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	TopP        float64           `json:"top_p,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
	Extra       map[string]any    `json:"-"` // campos adicionales que se pasan tal cual
}

// ChatCompletionResponse es el formato de respuesta de chat completions.
type ChatCompletionResponse struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"` // "chat.completion"
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	Choices           []ChatChoice    `json:"choices"`
	Usage             *Usage          `json:"usage,omitempty"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
}

// ChatMessage representa un mensaje en la conversación.
type ChatMessage struct {
	Role       string    `json:"role"`
	Content    any       `json:"content"` // string o []ContentPart
	Name       string    `json:"name,omitempty"`
}

// ContentPart permite contenido multimodal (aunque tag-mule solo usa texto).
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ChatChoice es una opción de respuesta.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage contiene información de tokens consumidos.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}