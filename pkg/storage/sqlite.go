// Package storage — implementación SQLite del almacenamiento.
//
// Almacena archivos JSONL y lotes (batches) en una base de datos SQLite
// para que los trabajos pendientes no se pierdan al reiniciar el servicio.
package storage

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implementa Store usando SQLite como backend.
// Todos los métodos son thread-safe porque SQLite lo maneja internamente.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore abre (o crea) la base de datos y crea las tablas necesarias.
// dbPath es la ruta al archivo .db (ej: "./data/tag-mule.db").
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("abriendo base de datos: %w", err)
	}

	// Configurar pool — SQLite va bien con 1 conexión gracias a WAL mode
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrando esquema: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Close cierra la base de datos.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// ─── Esquema ─────────────────────────────────────────────────────────────

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			id          TEXT PRIMARY KEY,
			filename    TEXT NOT NULL DEFAULT '',
			data        BLOB NOT NULL,
			purpose     TEXT NOT NULL DEFAULT 'batch',
			created_at  INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS batches (
			id                  TEXT PRIMARY KEY,
			endpoint            TEXT NOT NULL DEFAULT '/v1/chat/completions',
			completion_window   TEXT NOT NULL DEFAULT '24h',
			input_file_id       TEXT NOT NULL,
			output_file_id      TEXT NOT NULL DEFAULT '',
			error_file_id       TEXT NOT NULL DEFAULT '',
			status              TEXT NOT NULL DEFAULT 'pending',
			error               TEXT NOT NULL DEFAULT '',
			created_at          INTEGER NOT NULL,
			in_progress_at      INTEGER NOT NULL DEFAULT 0,
			completed_at        INTEGER NOT NULL DEFAULT 0,
			failed_at           INTEGER NOT NULL DEFAULT 0,
			total_requests      INTEGER NOT NULL DEFAULT 0,
			completed_requests  INTEGER NOT NULL DEFAULT 0,
			failed_requests     INTEGER NOT NULL DEFAULT 0
		);

		CREATE INDEX IF NOT EXISTS idx_batches_status ON batches(status);
	`)
	return err
}

// ─── Files ───────────────────────────────────────────────────────────────

// SaveFile almacena un archivo. Si ya existe, lo sobrescribe.
func (s *SQLiteStore) SaveFile(record FileRecord) string {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO files (id, filename, data, purpose, created_at) VALUES (?, ?, ?, ?, ?)`,
		record.ID, record.Filename, record.Bytes, record.Purpose, record.CreatedAt,
	)
	if err != nil {
		// En producción usaríamos un logger, pero para no acoplar,
		// el panic es razonable aquí (fallo de disco ≈ fatal).
		panic(fmt.Sprintf("sqlite: SaveFile: %v", err))
	}
	return record.ID
}

// GetFile recupera un archivo por ID.
func (s *SQLiteStore) GetFile(id string) (FileRecord, bool) {
	var r FileRecord
	var bytes []byte
	err := s.db.QueryRow(
		`SELECT id, filename, data, purpose, created_at FROM files WHERE id = ?`, id,
	).Scan(&r.ID, &r.Filename, &bytes, &r.Purpose, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return FileRecord{}, false
	}
	if err != nil {
		panic(fmt.Sprintf("sqlite: GetFile: %v", err))
	}
	r.Bytes = bytes
	return r, true
}

// ─── Batches ─────────────────────────────────────────────────────────────

// SaveBatch almacena un lote. Si ya existe, lo sobrescribe.
func (s *SQLiteStore) SaveBatch(record BatchRecord) string {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO batches
			(id, endpoint, completion_window, input_file_id, output_file_id,
			 error_file_id, status, error, created_at, in_progress_at,
			 completed_at, failed_at, total_requests, completed_requests, failed_requests)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Endpoint, record.CompletionWindow, record.InputFileID,
		record.OutputFileID, record.ErrorFileID, string(record.Status),
		record.Error, record.CreatedAt, record.InProgressAt, record.CompletedAt,
		record.FailedAt, record.TotalRequests, record.CompletedRequests, record.FailedRequests,
	)
	if err != nil {
		panic(fmt.Sprintf("sqlite: SaveBatch: %v", err))
	}
	return record.ID
}

// GetBatch recupera un lote por ID.
func (s *SQLiteStore) GetBatch(id string) (BatchRecord, bool) {
	var r BatchRecord
	var status string
	err := s.db.QueryRow(`
		SELECT id, endpoint, completion_window, input_file_id, output_file_id,
		       error_file_id, status, error, created_at, in_progress_at,
		       completed_at, failed_at, total_requests, completed_requests, failed_requests
		FROM batches WHERE id = ?`, id,
	).Scan(
		&r.ID, &r.Endpoint, &r.CompletionWindow, &r.InputFileID, &r.OutputFileID,
		&r.ErrorFileID, &status, &r.Error, &r.CreatedAt, &r.InProgressAt,
		&r.CompletedAt, &r.FailedAt, &r.TotalRequests, &r.CompletedRequests, &r.FailedRequests,
	)
	if err == sql.ErrNoRows {
		return BatchRecord{}, false
	}
	if err != nil {
		panic(fmt.Sprintf("sqlite: GetBatch: %v", err))
	}
	r.Status = BatchStatus(status)
	return r, true
}

// UpdateBatch aplica una función de actualización atómica sobre un lote en DB.
// Usa una transacción SQLite para garantizar la atomicidad.
func (s *SQLiteStore) UpdateBatch(id string, fn func(*BatchRecord)) bool {
	tx, err := s.db.Begin()
	if err != nil {
		panic(fmt.Sprintf("sqlite: UpdateBatch begin tx: %v", err))
	}
	defer tx.Rollback() // no-op si ya se hizo commit

	// Leer registro actual dentro de la transacción.
	// SQLite no soporta FOR UPDATE, pero con SetMaxOpenConns(1) + WAL mode
	// la transacción ya es atómica.
	var r BatchRecord
	var status string
	err = tx.QueryRow(`
		SELECT id, endpoint, completion_window, input_file_id, output_file_id,
		       error_file_id, status, error, created_at, in_progress_at,
		       completed_at, failed_at, total_requests, completed_requests, failed_requests
		FROM batches WHERE id = ?`, id,
	).Scan(
		&r.ID, &r.Endpoint, &r.CompletionWindow, &r.InputFileID, &r.OutputFileID,
		&r.ErrorFileID, &status, &r.Error, &r.CreatedAt, &r.InProgressAt,
		&r.CompletedAt, &r.FailedAt, &r.TotalRequests, &r.CompletedRequests, &r.FailedRequests,
	)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		panic(fmt.Sprintf("sqlite: UpdateBatch select: %v", err))
	}
	r.Status = BatchStatus(status)

	// Aplicar la mutación
	fn(&r)

	// Escribir de vuelta
	_, err = tx.Exec(`
		UPDATE batches SET
			endpoint=?, completion_window=?, input_file_id=?, output_file_id=?,
			error_file_id=?, status=?, error=?, created_at=?, in_progress_at=?,
			completed_at=?, failed_at=?, total_requests=?, completed_requests=?,
			failed_requests=?
		WHERE id=?`,
		r.Endpoint, r.CompletionWindow, r.InputFileID, r.OutputFileID,
		r.ErrorFileID, string(r.Status), r.Error, r.CreatedAt, r.InProgressAt,
		r.CompletedAt, r.FailedAt, r.TotalRequests, r.CompletedRequests,
		r.FailedRequests, id,
	)
	if err != nil {
		panic(fmt.Sprintf("sqlite: UpdateBatch update: %v", err))
	}

	return tx.Commit() == nil
}

// ─── Limpieza ────────────────────────────────────────────────────────────

// DeleteOldBatches elimina lotes completados/fallidos anteriores a la fecha dada.
// Útil para tareas de mantenimiento.
func (s *SQLiteStore) DeleteOldBatches(before time.Time) (int64, error) {
	ts := before.UnixMilli()
	res, err := s.db.Exec(
		`DELETE FROM batches WHERE status IN ('completed', 'failed', 'expired') AND completed_at > 0 AND completed_at < ?`,
		ts,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOldFiles elimina archivos anteriores a la fecha dada (que no estén referenciados por batches activos).
func (s *SQLiteStore) DeleteOldFiles(before time.Time) (int64, error) {
	ts := before.UnixMilli()
	res, err := s.db.Exec(`
		DELETE FROM files WHERE created_at < ? AND id NOT IN (
			SELECT input_file_id FROM batches WHERE status IN ('pending', 'processing')
			UNION
			SELECT output_file_id FROM batches WHERE output_file_id != ''
		)
	`, ts)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─── Reset (útil para testing) ───────────────────────────────────────────

// Reset elimina todos los registros. Solo para testing.
func (s *SQLiteStore) Reset() {
	_, _ = s.db.Exec("DELETE FROM files")
	_, _ = s.db.Exec("DELETE FROM batches")
}
