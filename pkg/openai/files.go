package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tag-mule/tag-mule/pkg/storage"
)

// FileHandler maneja los endpoints de archivos JSONL:
//   - POST   /v1/files           → subir archivo
//   - GET    /v1/files/{id}/content → descargar archivo
type FileHandler struct {
	store storage.Store
}

// NewFileHandler crea un nuevo FileHandler con el store proporcionado.
func NewFileHandler(store storage.Store) *FileHandler {
	return &FileHandler{store: store}
}

// generateID genera un ID pseudoaleatorio con el prefijo dado.
// Usa crypto/rand para evitar colisiones.
func generateID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// --- POST /v1/files ---

// CreateFile recibe un archivo vía multipart form, lo almacena en
// memoria y responde con el objeto file compatible con OpenAI.
//
// El cliente debe enviar el archivo en el campo "file" del formulario
// multipart, con Content-Type adecuado. El campo opcional "purpose"
// defaults a "batch".
func (h *FileHandler) CreateFile(w http.ResponseWriter, r *http.Request) {
	// Limitar el tamaño del body a 50 MB para evitar abuso
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	if err := r.ParseMultipartForm(50 * 1024 * 1024); err != nil {
		slog.Error("error parseando formulario multipart", "error", err)
		writeError(w, http.StatusBadRequest,
			"invalid_request_error",
			"Error al parsear el formulario. Verificá que el archivo no supere los 50 MB.")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest,
			"invalid_request_error",
			"Campo 'file' no encontrado. El archivo debe enviarse como multipart/form-data con key='file'.")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		slog.Error("error leyendo archivo subido", "error", err)
		writeError(w, http.StatusInternalServerError,
			"server_error",
			"Error interno al leer el archivo subido.")
		return
	}

	// Validar que el archivo no esté vacío
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest,
			"invalid_request_error",
			"El archivo está vacío.")
		return
	}

	purpose := r.FormValue("purpose")
	if purpose == "" {
		purpose = "batch"
	}

	fileID := generateID("file-")
	now := time.Now().UnixMilli()

	record := storage.FileRecord{
		ID:        fileID,
		Filename:  header.Filename,
		Bytes:     data,
		Purpose:   purpose,
		CreatedAt: now,
	}

	h.store.SaveFile(record)

	slog.Info("archivo subido correctamente",
		"file_id", fileID,
		"filename", header.Filename,
		"bytes", len(data),
		"purpose", purpose,
	)

	resp := FileUploadResponse{
		ID:        fileID,
		Object:    "file",
		Bytes:     len(data),
		CreatedAt: now,
		Filename:  header.Filename,
		Purpose:   purpose,
	}

	writeJSON(w, http.StatusOK, resp)
}

// --- GET /v1/files/{id}/content ---

// GetFileContent retorna el contenido del archivo JSONL solicitado.
// El Content-Type se establece a "application/jsonl" para que el
// cliente pueda parsear línea por línea.
func (h *FileHandler) GetFileContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	record, ok := h.store.GetFile(id)
	if !ok {
		writeError(w, http.StatusNotFound,
			"file_not_found",
			fmt.Sprintf("El archivo '%s' no existe o fue eliminado.", id))
		return
	}

	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s_output.jsonl"`, id))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(record.Bytes)))
	w.WriteHeader(http.StatusOK)
	w.Write(record.Bytes)

	slog.Debug("contenido de archivo entregado",
		"file_id", id,
		"bytes", len(record.Bytes),
	)
}

// --- Helpers de respuesta HTTP ---

// apiError es el formato estándar de error compatible con OpenAI.
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
		Param   any    `json:"param,omitempty"`
	} `json:"error"`
}

// writeError responde con un error JSON en formato OpenAI.
func writeError(w http.ResponseWriter, status int, errType, message string) {
	var e apiError
	e.Error.Message = message
	e.Error.Type = errType
	e.Error.Code = errType
	e.Error.Param = nil

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e)
}

// writeJSON responde con un objeto JSON y el código de estado indicado.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}