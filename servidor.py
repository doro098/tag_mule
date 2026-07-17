"""
tag-mule — Servidor de prueba con backends intercambiables.

Lee configuración de variables de entorno (o .env).
Soporta múltiples escenarios (cada uno con su system prompt)
y múltiples backends (tag-mule / openai-compatible).

Configuración vía .env:
  BACKEND=tag-mule|openai
  TAG_MULE_URL=http://localhost:8080
  MODEL=qwen2.5:1.5b
  OPENAI_BASE_URL=https://api.z.ai/api/paas/v4/
  OPENAI_API_KEY=sk-...
  OPENAI_MODEL=glm-4.5-flash
  DB_PATH=db.db
"""

import os
import sqlite3
import threading
import time
import json
import requests
from flask import Flask, request, jsonify, send_file

app = Flask(__name__)

# ── Config desde .env ────────────────────────────────────────
BACKEND = os.getenv("BACKEND", "tag-mule")  # "tag-mule" | "openai"
TAG_MULE_URL = os.getenv("TAG_MULE_URL", "http://localhost:8080")
MODEL = os.getenv("MODEL", "qwen2.5:1.5b")
OPENAI_BASE_URL = os.getenv("OPENAI_BASE_URL", "").rstrip("/")
OPENAI_API_KEY = os.getenv("OPENAI_API_KEY", "")
OPENAI_MODEL = os.getenv("OPENAI_MODEL", "")
DB_PATH = os.getenv("DB_PATH", "db.db")

# ── Escenarios ───────────────────────────────────────────────
ESCENARIOS = {
    "simular-servicio": {
        "label": "Simular Servicio",
        "desc": "Atención al cliente genérica",
        "prompt": (
            "Eres un asistente de atención al cliente amable y profesional. "
            "Respondé de forma natural y servicial al mensaje del cliente."
        ),
    },
    "tureparto": {
        "label": "TuReparto",
        "desc": "Reparto de bidones de agua",
        "prompt": (
            "Eres un extractor de etiquetas para un sistema de reparto de bidones de agua. "
            "Dado un mensaje de un cliente, extraé la información relevante.\n"
            "Respondé SOLO con un JSON válido, sin texto adicional:\n"
            '{\n'
            '  "tags": ["pedido", "cliente"],\n'
            '  "entidades": {\n'
            '    "nombre": "nombre de la persona",\n'
            '    "producto": "producto mencionado",\n'
            '    "cantidad": 0\n'
            '  },\n'
            '  "intencion": "pedido | consulta | otra"\n'
            '}'
        ),
    },
    "mulinku": {
        "label": "Mulinku",
        "desc": "Análisis de enlaces",
        "prompt": (
            "Eres un analizador de enlaces y contenido web. "
            "Dado un texto o URL, extraé los enlaces, sus dominios y un resumen "
            "del contenido. Respondé SOLO con un JSON válido."
        ),
    },
}


# ── DB ────────────────────────────────────────────────────────
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
            escenario  TEXT DEFAULT 'simular-servicio',
            status     TEXT DEFAULT 'pending',
            file_id    TEXT,
            batch_id   TEXT,
            resultado  TEXT,
            error      TEXT,
            created_at TEXT,
            updated_at TEXT
        )
    """)
    # Migrar DB vieja si no tiene columna escenario
    try:
        conn.execute("ALTER TABLE envios ADD COLUMN escenario TEXT DEFAULT 'simular-servicio'")
    except sqlite3.OperationalError:
        pass  # ya existe
    conn.commit()
    conn.close()


def add_log(sub_id, msg):
    ts = time.strftime("%H:%M:%S")
    print(f"  [{ts}] [{sub_id}] {msg}")


# ── Procesamiento ──────────────────────────────────────────────
def procesar(sub_id, escenario):
    conn = get_db()
    now = lambda: time.strftime("%Y-%m-%d %H:%M:%S")

    try:
        row = conn.execute("SELECT mensaje FROM envios WHERE id=?", (sub_id,)).fetchone()
        if not row:
            return

        mensaje = row["mensaje"]
        esc_info = ESCENARIOS.get(escenario, ESCENARIOS["simular-servicio"])
        system_prompt = esc_info["prompt"]

        conn.execute("UPDATE envios SET status='processing', updated_at=? WHERE id=?", (now(), sub_id))
        conn.commit()
        add_log(sub_id, f"backend={BACKEND} escenario={escenario}")

        # ── BACKEND: OpenAI-compatible ────────────────────────
        if BACKEND == "openai":
            url = f"{OPENAI_BASE_URL}/chat/completions"
            headers = {
                "Content-Type": "application/json",
                "Authorization": f"Bearer {OPENAI_API_KEY}",
            }
            body = {
                "model": OPENAI_MODEL or MODEL,
                "messages": [
                    {"role": "system", "content": system_prompt},
                    {"role": "user", "content": mensaje},
                ],
            }

            add_log(sub_id, f"POST {url} ...")
            r = requests.post(url, json=body, headers=headers, timeout=120)
            if not r.ok:
                raise Exception(f"API error: {r.status} {r.text[:300]}")

            data = r.json()
            resultado = (
                data.get("choices", [{}])[0]
                .get("message", {})
                .get("content", "")
            )
            conn.execute(
                "UPDATE envios SET resultado=?, status='done', updated_at=? WHERE id=?",
                (resultado, now(), sub_id),
            )
            conn.commit()
            add_log(sub_id, f"resultado: {resultado[:150]}")
            return

        # ── BACKEND: tag-mule (Batch API) ─────────────────────
        base = TAG_MULE_URL.rstrip("/")

        # 1) Armar JSONL con system prompt
        jsonl = json.dumps({
            "custom_id": sub_id,
            "method": "POST",
            "url": "/v1/chat/completions",
            "body": {
                "model": MODEL,
                "messages": [
                    {"role": "system", "content": system_prompt},
                    {"role": "user", "content": mensaje},
                ],
            }
        }, ensure_ascii=False) + "\n"

        # 2) Subir archivo
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
            conn.execute("UPDATE envios SET status=?, updated_at=? WHERE id=?", (status, now(), sub_id))
            conn.commit()
            if status in ("completed", "failed", "expired", "cancelled"):
                break

        # 5) Descargar resultado
        if batch and batch.get("output_file_id"):
            add_log(sub_id, "descargando resultado...")
            r = requests.get(f"{base}/v1/files/{batch['output_file_id']}/content", timeout=30)
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
                add_log(sub_id, f"resultado: {resultado[:150]}")
                break
        elif batch and batch.get("error"):
            err = batch["error"]["message"]
            conn.execute("UPDATE envios SET error=? WHERE id=?", (err, sub_id))
            conn.commit()
            add_log(sub_id, f"ERROR: {err}")

    except Exception as e:
        conn.execute("UPDATE envios SET status='error', error=? WHERE id=?", (str(e), sub_id))
        conn.commit()
        add_log(sub_id, f"EXCEPTION: {e}")

    finally:
        conn.close()


# ── Rutas ──────────────────────────────────────────────────────
@app.route("/")
def index():
    return send_file("ejemplo.html")


@app.route("/api/escenarios")
def listar_escenarios():
    """Devuelve la lista de escenarios disponibles."""
    return jsonify([
        {"id": k, "label": v["label"], "desc": v["desc"]}
        for k, v in ESCENARIOS.items()
    ])


@app.route("/api/config")
def ver_config():
    """Devuelve la config actual (sin api key)."""
    return jsonify({
        "backend": BACKEND,
        "model": MODEL,
        "escenarios": list(ESCENARIOS.keys()),
    })


@app.route("/api/enviar", methods=["POST"])
def enviar():
    data = request.json or {}
    mensaje = data.get("mensaje", "").strip()
    escenario = data.get("escenario", "simular-servicio").strip()

    if not mensaje:
        return jsonify({"error": "mensaje vacío"}), 400
    if escenario not in ESCENARIOS:
        return jsonify({"error": f"escenario '{escenario}' no válido"}), 400

    sub_id = f"sub-{int(time.time() * 1000)}"
    ahora = time.strftime("%Y-%m-%d %H:%M:%S")

    conn = get_db()
    conn.execute(
        "INSERT INTO envios (id, mensaje, escenario, status, created_at, updated_at) VALUES (?,?,?,?,?,?)",
        (sub_id, mensaje, escenario, "pending", ahora, ahora),
    )
    conn.commit()
    conn.close()

    threading.Thread(target=procesar, args=(sub_id, escenario), daemon=True).start()

    add_log(sub_id, f"recibido [{escenario}]: {mensaje[:80]}")
    return jsonify({"id": sub_id, "status": "pending", "escenario": escenario})


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
        "escenario": row["escenario"],
        "status": row["status"],
        "file_id": row["file_id"],
        "batch_id": row["batch_id"],
        "resultado": row["resultado"],
        "error": row["error"],
        "created_at": row["created_at"],
        "updated_at": row["updated_at"],
    })


# ── Main ───────────────────────────────────────────────────────
if __name__ == "__main__":
    init_db()
    print(f"tag-mule test server → http://localhost:5001")
    print(f"  backend: {BACKEND}")
    print(f"  modelo:  {MODEL}")
    print(f"  escenarios: {', '.join(ESCENARIOS.keys())}")
    app.run(host="0.0.0.0", port=5001, debug=False)
