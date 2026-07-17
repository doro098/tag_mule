"""
tag-mule — Servidor de test.
Sirve ejemplo.html, recibe envíos, los manda a tag-mule
y guarda todo en db.db (SQLite).
"""

import sqlite3
import threading
import time
import json
import requests
from flask import Flask, request, jsonify, send_file

app = Flask(__name__)

TAG_MULE_URL = "http://localhost:8080"
MODEL = "qwen2.5:1.5b"
DB_PATH = "db.db"


# ── DB ────────────────────────────────────────────────────────────────────

def get_db():
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    return conn


def init_db():
    conn = get_db()
    conn.execute("""
        CREATE TABLE IF NOT EXISTS envios (
            id         TEXT PRIMARY KEY,
            mensaje    TEXT,
            status     TEXT DEFAULT 'pending',
            file_id    TEXT,
            batch_id   TEXT,
            resultado  TEXT,
            error      TEXT,
            created_at TEXT,
            updated_at TEXT
        )
    """)
    conn.commit()
    conn.close()


def add_log(sub_id, msg):
    """Escribe una línea de log en consola."""
    ts = time.strftime("%H:%M:%S")
    print(f"  [{ts}] [{sub_id}] {msg}")


# ── Background: procesa el envío contra tag-mule ─────────────────────────

def procesar(sub_id):
    conn = get_db()
    now = lambda: time.strftime("%Y-%m-%d %H:%M:%S")

    try:
        row = conn.execute("SELECT mensaje FROM envios WHERE id=?", (sub_id,)).fetchone()
        if not row:
            return

        mensaje = row["mensaje"]
        base = TAG_MULE_URL.rstrip("/")

        # → processing
        conn.execute("UPDATE envios SET status='processing', updated_at=? WHERE id=?", (now(), sub_id))
        conn.commit()
        add_log(sub_id, "procesando...")

        # 1) Armar JSONL
        jsonl = json.dumps({
            "custom_id": sub_id,
            "method": "POST",
            "url": "/v1/chat/completions",
            "body": {
                "model": MODEL,
                "messages": [{"role": "user", "content": mensaje}],
            }
        }, ensure_ascii=False) + "\n"

        # 2) Subir archivo a tag-mule
        add_log(sub_id, "POST /v1/files ...")
        r = requests.post(
            f"{base}/v1/files",
            files={"file": ("input.jsonl", jsonl, "application/jsonl")},
            data={"purpose": "batch"},
            timeout=10,
        )
        if not r.ok:
            raise Exception(f"upload falló: {r.status} {r.text[:200]}")
        file_id = r.json()["id"]
        conn.execute("UPDATE envios SET file_id=? WHERE id=?", (file_id, sub_id))
        conn.commit()
        add_log(sub_id, f"file_id={file_id}")

        # 3) Crear batch
        add_log(sub_id, "POST /v1/batches ...")
        r = requests.post(
            f"{base}/v1/batches",
            json={"input_file_id": file_id, "endpoint": "/v1/chat/completions"},
            timeout=10,
        )
        if not r.ok:
            raise Exception(f"batch falló: {r.status} {r.text[:200]}")
        batch_id = r.json()["id"]
        conn.execute("UPDATE envios SET batch_id=? WHERE id=?", (batch_id, sub_id))
        conn.commit()
        add_log(sub_id, f"batch_id={batch_id}")

        # 4) Polling
        add_log(sub_id, "polling...")
        batch = None
        for _ in range(300):
            time.sleep(2)
            r = requests.get(f"{base}/v1/batches/{batch_id}", timeout=10)
            batch = r.json()
            status = batch["status"]
            conn.execute(
                "UPDATE envios SET status=?, updated_at=? WHERE id=?",
                (status, now(), sub_id),
            )
            conn.commit()
            if status in ("completed", "failed", "expired", "cancelled"):
                break

        # 5) Descargar resultado
        if batch and batch.get("output_file_id"):
            add_log(sub_id, "descargando resultado...")
            r = requests.get(
                f"{base}/v1/files/{batch['output_file_id']}/content",
                timeout=30,
            )
            for line in r.text.strip().split("\n"):
                if not line.strip():
                    continue
                resultado = (
                    json.loads(line)
                    .get("response", {})
                    .get("body", {})
                    .get("choices", [{}])[0]
                    .get("message", {})
                    .get("content", "")
                )
                conn.execute(
                    "UPDATE envios SET resultado=?, status='done' WHERE id=?",
                    (resultado, sub_id),
                )
                conn.commit()
                add_log(sub_id, f"resultado: {resultado[:100]}")
                break
        elif batch and batch.get("error"):
            err = batch["error"]["message"]
            conn.execute("UPDATE envios SET error=? WHERE id=?", (err, sub_id))
            conn.commit()
            add_log(sub_id, f"ERROR: {err}")

    except Exception as e:
        conn.execute(
            "UPDATE envios SET status='error', error=? WHERE id=?",
            (str(e), sub_id),
        )
        conn.commit()
        add_log(sub_id, f"EXCEPTION: {e}")

    finally:
        conn.close()


# ── Rutas ────────────────────────────────────────────────────────────────

@app.route("/")
def index():
    return send_file("ejemplo.html")


@app.route("/api/enviar", methods=["POST"])
def enviar():
    data = request.json or {}
    mensaje = data.get("mensaje", "").strip()
    if not mensaje:
        return jsonify({"error": "mensaje vacío"}), 400

    sub_id = f"sub-{int(time.time() * 1000)}"
    ahora = time.strftime("%Y-%m-%d %H:%M:%S")

    conn = get_db()
    conn.execute(
        "INSERT INTO envios (id, mensaje, status, created_at, updated_at) VALUES (?,?,?,?,?)",
        (sub_id, mensaje, "pending", ahora, ahora),
    )
    conn.commit()
    conn.close()

    threading.Thread(target=procesar, args=(sub_id,), daemon=True).start()

    add_log(sub_id, f"recibido: {mensaje[:80]}")
    return jsonify({"id": sub_id, "status": "pending"})


@app.route("/api/estado/<sub_id>")
def estado(sub_id):
    conn = get_db()
    row = conn.execute("SELECT * FROM envios WHERE id=?", (sub_id,)).fetchone()
    conn.close()

    if not row:
        return jsonify({"error": "no encontrado"}), 404

    return jsonify({
        "id": row["id"],
        "mensaje": row["mensaje"],
        "status": row["status"],
        "file_id": row["file_id"],
        "batch_id": row["batch_id"],
        "resultado": row["resultado"],
        "error": row["error"],
        "created_at": row["created_at"],
        "updated_at": row["updated_at"],
    })


# ── Main ─────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    init_db()
    print("tag-mule test server → http://localhost:5001")
    app.run(host="0.0.0.0", port=5001, debug=False)