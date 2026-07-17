// Package storage implementa un almacenamiento en memoria protegido por
// sync.RWMutex para persistir los estados de archivos y lotes.
// Es ultra-simple, sin dependencias externas.
package storage

import (
        "sync"
)

// FileRecord representa un archivo JSONL almacenado.
type FileRecord struct {
        ID        string
        Filename  string
        Bytes     []byte
        Purpose   string
        CreatedAt int64 // unix timestamp en milisegundos
}

// BatchStatus define los estados posibles de un lote.
type BatchStatus string

const (
        BatchStatusPending    BatchStatus = "pending"
        BatchStatusProcessing BatchStatus = "processing"
        BatchStatusCompleted  BatchStatus = "completed"
        BatchStatusFailed     BatchStatus = "failed"
        BatchStatusExpired    BatchStatus = "expired"
        BatchStatusCancelling BatchStatus = "cancelling"
        BatchStatusCancelled  BatchStatus = "cancelled"
)

// BatchRecord representa un lote de procesamiento.
type BatchRecord struct {
        ID                string
        Endpoint          string // ej: "/v1/chat/completions"
        CompletionWindow  string // ej: "24h"
        InputFileID       string
        OutputFileID      string
        ErrorFileID       string
        Status            BatchStatus
        Error             string
        CreatedAt         int64 // unix timestamp en milisegundos
        InProgressAt      int64
        CompletedAt       int64 // unix timestamp en milisegundos, 0 si no terminó
        FailedAt          int64
        TotalRequests     int
        CompletedRequests int
        FailedRequests    int
}

// Store define la interfaz para el almacenamiento.
type Store interface {
        // Archivos
        SaveFile(record FileRecord) string
        GetFile(id string) (FileRecord, bool)

        // Lotes
        SaveBatch(record BatchRecord) string
        GetBatch(id string) (BatchRecord, bool)
        UpdateBatch(id string, fn func(*BatchRecord)) bool
}

// MemoryStore es una implementación de Store que usa mapas en memoria.
type MemoryStore struct {
        mu    sync.RWMutex
        files map[string]FileRecord
        batch map[string]BatchRecord
}

// NewMemoryStore crea un nuevo MemoryStore inicializado.
func NewMemoryStore() *MemoryStore {
        return &MemoryStore{
                files: make(map[string]FileRecord),
                batch: make(map[string]BatchRecord),
        }
}

// SaveFile almacena un archivo y retorna su ID.
// Si el ID ya existe, lo sobrescribe.
func (s *MemoryStore) SaveFile(record FileRecord) string {
        s.mu.Lock()
        defer s.mu.Unlock()
        s.files[record.ID] = record
        return record.ID
}

// GetFile busca un archivo por ID. Retorna el registro y true si existe.
func (s *MemoryStore) GetFile(id string) (FileRecord, bool) {
        s.mu.RLock()
        defer s.mu.RUnlock()
        f, ok := s.files[id]
        return f, ok
}

// SaveBatch almacena un lote y retorna su ID.
func (s *MemoryStore) SaveBatch(record BatchRecord) string {
        s.mu.Lock()
        defer s.mu.Unlock()
        s.batch[record.ID] = record
        return record.ID
}

// GetBatch busca un lote por ID. Retorna el registro y true si existe.
func (s *MemoryStore) GetBatch(id string) (BatchRecord, bool) {
        s.mu.RLock()
        defer s.mu.RUnlock()
        b, ok := s.batch[id]
        return b, ok
}

// UpdateBatch aplica una función de actualización atómica sobre un lote.
// Si el lote no existe, retorna false.
func (s *MemoryStore) UpdateBatch(id string, fn func(*BatchRecord)) bool {
        s.mu.Lock()
        defer s.mu.Unlock()
        b, ok := s.batch[id]
        if !ok {
                return false
        }
        fn(&b)
        s.batch[id] = b
        return true
}