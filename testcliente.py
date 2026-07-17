"""
tag-mule — Cliente de consola para testear servidor.py

Uso:
    python testcliente.py "https://ejemplo.com"
    python testcliente.py "Extraé entidades de este mensaje"
"""

import sys
import time
import requests

BASE = "http://localhost:5001"


def main():
    if len(sys.argv) > 1:
        mensaje = " ".join(sys.argv[1:])
    else:
        mensaje = input("Link o mensaje: ").strip()

    if not mensaje:
        print("vacío.")
        sys.exit(1)

    # Enviar
    print(f"\nEnviando: {mensaje}")
    r = requests.post(f"{BASE}/api/enviar", json={"mensaje": mensaje})
    r.raise_for_status()
    data = r.json()
    sub_id = data["id"]
    print(f"ID: {sub_id}\n")

    # Polling
    while True:
        time.sleep(2)
        r = requests.get(f"{BASE}/api/estado/{sub_id}")
        s = r.json()
        status = s["status"]
        extra = s.get("error") or s.get("resultado") or ""
        if extra:
            extra = " | " + extra[:100]
        print(f"  [{status:12}]{extra}")
        if status in ("done", "error", "failed", "expired"):
            break

    if s.get("resultado"):
        print(f"\nRESULTADO: {s['resultado']}")


if __name__ == "__main__":
    main()