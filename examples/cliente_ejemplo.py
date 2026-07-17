"""
╔══════════════════════════════════════════════════════════════╗
║  tag-mule — Ejemplo de integración 🐫                       ║
║                                                            ║
║  Este script muestra cómo usar la API Batch de tag-mule     ║
║  desde cualquier aplicación Python.                         ║
║                                                            ║
║  🔄 Flujo:                                                  ║
║    1. Crear un JSONL con tu mensaje                         ║
║    2. Subirlo a tag-mule  (POST /v1/files)                  ║
║    3. Crear un batch       (POST /v1/batches)               ║
║    4. Consultar hasta que termine (GET /v1/batches/{id})    ║
║    5. Descargar el resultado (GET /v1/files/{id}/content)   ║
║                                                            ║
║  ▶️  Uso rápido:                                            ║
║      python cliente_ejemplo.py "tu mensaje acá"             ║
║                                                            ║
║  📦 Solo requiere: requests                                 ║
║      pip install requests                                   ║
╚══════════════════════════════════════════════════════════════╝
"""

import json
import sys
import time
from typing import Optional
from urllib.parse import urljoin

import requests


# ────────────────────────────────────────────────────────────────
#  CONFIGURACIÓN
# ────────────────────────────────────────────────────────────────

# Cambiá esto si tag-mule corre en otro host/puerto.
TAG_MULE_URL = "http://localhost:8080"

# Modelo a usar en Ollama (o proveedor externo).
# En Ollama podés ver los modelos disponibles con:
#   docker exec ollama ollama list
MODELO = "llama3.2:3b"


# ────────────────────────────────────────────────────────────────
#  PASO 1 — Armar el JSONL
# ────────────────────────────────────────────────────────────────

def armar_jsonl(mensaje: str, custom_id: Optional[str] = None) -> str:
    """
    Crea una línea en formato JSONL compatible con la API Batch de OpenAI.

    tag-mule entiende este formato exacto. Cada línea representa una
    petición individual al LLM dentro del lote.

    Parámetros:
        mensaje   — El texto a procesar (ej: un link, una consulta, etc.)
        custom_id — ID único para identificar esta petición en el resultado.
                    Si no se pasa, se genera uno automáticamente.

    Devuelve:
        Un string JSONL (una sola línea + salto de línea).
    """
    if custom_id is None:
        custom_id = f"req-{int(time.time() * 1000)}"

    linea = {
        "custom_id": custom_id,
        "method": "POST",
        "url": "/v1/chat/completions",
        "body": {
            "model": MODELO,
            "messages": [
                {"role": "user", "content": mensaje}
            ],
            # Podés agregar más parámetros si querés:
            # "temperature": 0.7,
            # "max_tokens": 500,
        },
    }

    return json.dumps(linea, ensure_ascii=False) + "\n"


# ────────────────────────────────────────────────────────────────
#  PASO 2 — Subir el archivo a tag-mule
# ────────────────────────────────────────────────────────────────

def subir_archivo(jsonl_content: str) -> str:
    """
    Sube un archivo JSONL a tag-mule.

    El archivo se envía como multipart/form-data, igual que en la API
    de OpenAI (campo 'file' con el contenido, campo 'purpose' con el uso).

    Parámetros:
        jsonl_content — El contenido JSONL generado en el paso anterior.

    Devuelve:
        El ID del archivo (ej: "file-abc123...") para usarlo en el batch.

    Lanza:
        Exception si la subida falla.
    """
    url = urljoin(TAG_MULE_URL, "/v1/files")

    # La API espera un archivo como multipart
    archivos = {
        "file": ("input.jsonl", jsonl_content.encode("utf-8"), "application/jsonl"),
    }
    datos = {"purpose": "batch"}

    respuesta = requests.post(url, files=archivos, data=datos, timeout=30)

    if not respuesta.ok:
        raise Exception(
            f"Error subiendo archivo: {respuesta.status_code}\n"
            f"{respuesta.text[:300]}"
        )

    file_id = respuesta.json()["id"]
    print(f"  ✅ Archivo subido: {file_id}")
    return file_id


# ────────────────────────────────────────────────────────────────
#  PASO 3 — Crear el batch
# ────────────────────────────────────────────────────────────────

def crear_batch(file_id: str) -> str:
    """
    Crea un batch en tag-mule para procesar el archivo subido.

    tag-mule va a tomar el JSONL, enviar cada línea al LLM (Ollama
    o proveedor externo), y generar un archivo de salida con las
    respuestas.

    Parámetros:
        file_id — El ID del archivo subido en el paso anterior.

    Devuelve:
        El ID del batch (ej: "batch_xyz...") para consultar el estado.

    Lanza:
        Exception si la creación del batch falla.
    """
    url = urljoin(TAG_MULE_URL, "/v1/batches")

    body = {
        "input_file_id": file_id,
        "endpoint": "/v1/chat/completions",
        "completion_window": "24h",
    }

    respuesta = requests.post(url, json=body, timeout=10)

    if not respuesta.ok:
        raise Exception(
            f"Error creando batch: {respuesta.status_code}\n"
            f"{respuesta.text[:300]}"
        )

    batch_id = respuesta.json()["id"]
    print(f"  ✅ Batch creado:    {batch_id}")
    return batch_id


# ────────────────────────────────────────────────────────────────
#  PASO 4 — Esperar el resultado (polling)
# ────────────────────────────────────────────────────────────────

def esperar_resultado(batch_id: str, intervalo: int = 2) -> dict:
    """
    Consulta el estado del batch cada 'intervalo' segundos hasta
    que termina (completado, fallido, expirado o cancelado).

    Es exactamente lo mismo que hace la API de OpenAI Batch:
    el cliente hace GET al endpoint y tag-mule responde con el
    estado actual.

    Parámetros:
        batch_id  — El ID del batch creado en el paso anterior.
        intervalo — Cada cuántos segundos consultar (default: 2).

    Devuelve:
        El JSON completo del batch cuando termina.

    Lanza:
        Exception si el batch falla o expira.
    """
    url = urljoin(TAG_MULE_URL, f"/v1/batches/{batch_id}")

    estados_finales = {"completed", "failed", "expired", "cancelled"}

    print(f"  ⏳ Procesando... (consultando cada {intervalo}s)")
    barras = 0

    while True:
        respuesta = requests.get(url, timeout=10)
        if not respuesta.ok:
            raise Exception(
                f"Error consultando batch: {respuesta.status_code}\n"
                f"{respuesta.text[:200]}"
            )

        batch = respuesta.json()
        status = batch["status"]

        # Animación simple de progreso
        barras += 1
        progreso = "▓" * (barras % 10) + "░" * (10 - (barras % 10))
        print(f"  {progreso} [{status}]", end="\r")

        if status in estados_finales:
            print()  # salto de línea después de la animación
            if status == "completed":
                print(f"  ✅ Batch completado")
            else:
                error_msg = batch.get("error", {}).get("message", "Sin detalle")
                raise Exception(f"Batch {status}: {error_msg}")
            return batch

        time.sleep(intervalo)


# ────────────────────────────────────────────────────────────────
#  PASO 5 — Descargar el resultado
# ────────────────────────────────────────────────────────────────

def descargar_resultado(output_file_id: str) -> str:
    """
    Descarga el archivo JSONL de salida generado por tag-mule y
    extrae el contenido de texto de la primera respuesta exitosa.

    El archivo de salida tiene el mismo formato que OpenAI Batch:
    una línea por cada petición procesada, con el campo
    response.body.choices[0].message.content conteniendo el texto
    generado por el LLM.

    Parámetros:
        output_file_id — El ID del archivo de salida (viene en el
                         batch cuando status = "completed").

    Devuelve:
        El texto de la respuesta del LLM.
        Nota: si el batch tenía varias peticiones, solo devuelve
        el contenido de la primera respuesta exitosa.

    Lanza:
        Exception si no se encuentra contenido válido.
    """
    url = urljoin(TAG_MULE_URL, f"/v1/files/{output_file_id}/content")

    respuesta = requests.get(url, timeout=30)
    if not respuesta.ok:
        raise Exception(
            f"Error descargando resultado: {respuesta.status_code}\n"
            f"{respuesta.text[:200]}"
        )

    # El archivo de salida es JSONL, una línea por petición
    for linea in respuesta.text.strip().split("\n"):
        if not linea.strip():
            continue

        datos = json.loads(linea)

        # Si hay error en esta línea, lo mostramos pero seguimos
        if datos.get("error"):
            print(f"  ⚠️  Error en una línea: {datos['error']['message']}")
            continue

        # Extraer el contenido del mensaje del asistente
        contenido = (
            datos.get("response", {})
            .get("body", {})
            .get("choices", [{}])[0]
            .get("message", {})
            .get("content", "")
        )

        if contenido:
            return contenido

    raise Exception("No se encontró contenido en la respuesta")


# ────────────────────────────────────────────────────────────────
#  FUNCIÓN PRINCIPAL — Todo en uno
# ────────────────────────────────────────────────────────────────

def verificar_conexion() -> None:
    """Verifica que tag-mule esté accesible antes de arrancar."""
    url = urljoin(TAG_MULE_URL, "/health")
    try:
        resp = requests.get(url, timeout=5)
        if not resp.ok:
            raise Exception(f"health check respondió {resp.status_code}")
    except requests.ConnectionError:
        raise Exception(
            f"No se puede conectar a tag-mule en {TAG_MULE_URL}.\n"
            f"¿Está corriendo el contenedor? Probá:\n"
            f"  cd dtr/ && docker compose ps"
        )


def enviar_mensaje(mensaje: str) -> str:
    """
    Envía un mensaje a tag-mule y devuelve el resultado.

    Esta función ejecuta el flujo completo de la API Batch:
    JSONL → Upload → Batch → Poll → Download

    Es lo único que tu amigo necesita llamar desde su app.
    El resto del archivo es documentación y ejemplos.

    Parámetros:
        mensaje — El texto a procesar (máximo recomendado: ~1000 palabras).

    Devuelve:
        El texto generado por el LLM.

    Ejemplo:
        >>> resultado = enviar_mensaje("Resumí este artículo: ...")
        >>> print(resultado)
    """
    print(f"\n  🐫 tag-mule — Procesando mensaje...\n")
    print(f"  📝 Mensaje: {mensaje[:80]}{'...' if len(mensaje) > 80 else ''}")

    # 0) Verificar conectividad
    print(f"  ── Paso 0/5: Verificar conexión ──")
    verificar_conexion()
    print(f"  ✅ tag-mule responde en {TAG_MULE_URL}")

    # 1) Preparar el JSONL
    print(f"\n  ── Paso 1/5: Armar JSONL ──")
    jsonl = armar_jsonl(mensaje)

    # 2) Subir archivo
    print(f"  ── Paso 2/5: Subir archivo ──")
    file_id = subir_archivo(jsonl)

    # 3) Crear batch
    print(f"  ── Paso 3/5: Crear batch ──")
    batch_id = crear_batch(file_id)

    # 4) Esperar resultado
    print(f"  ── Paso 4/5: Esperar resultado ──")
    batch = esperar_resultado(batch_id)

    # 5) Descargar
    print(f"  ── Paso 5/5: Descargar resultado ──")
    resultado = descargar_resultado(batch["output_file_id"])

    print(f"\n  ── ✅ COMPLETADO ──\n")
    return resultado


# ────────────────────────────────────────────────────────────────
#  EJEMPLO DE USO DESDE CONSOLA
# ────────────────────────────────────────────────────────────────

def main():
    """Ejemplo de uso desde la terminal."""
    if len(sys.argv) > 1:
        mensaje = " ".join(sys.argv[1:])
    else:
        # Si no hay argumento, usar un mensaje de ejemplo
        mensaje = (
            "Decime tres funciones de la cebolla que la hagan "
            "interesante para la cocina."
        )
        print(f"\n  💡 Usá: python cliente_ejemplo.py 'tu mensaje acá'")
        print(f"  📌 Mostrando ejemplo con mensaje predeterminado:\n")
        print(f"     {mensaje}\n")

    try:
        resultado = enviar_mensaje(mensaje)
        print(f"\n{'─' * 60}")
        print(f"  RESPUESTA DEL LLM:")
        print(f"{'─' * 60}")
        print(f"\n{resultado}\n")
        print(f"{'─' * 60}\n")
    except Exception as e:
        print(f"\n  ❌ ERROR: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
