// Package env proporciona un parser minimalista de archivos .env
// utilizando únicamente la librería estándar de Go.
package env

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load lee un archivo .env y establece las variables de entorno
// correspondientes. Las líneas en blanco y las que comienzan con '#'
// se ignoran. Si el archivo no existe, retorna nil sin error
// (permitiendo que las variables se configuren vía el entorno del OS).
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("abriendo .env: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineno := 0
	for scanner.Scan() {
		lineno++
		line := strings.TrimSpace(scanner.Text())

		// Ignorar líneas vacías y comentarios
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Buscar el primer '=' para dividir clave y valor
		idx := strings.Index(line, "=")
		if idx == -1 {
			continue // línea sin '=', la saltamos
		}

		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])

		// Quitar comillas rodeantes si están presentes
		value = strings.Trim(value, `"'`)

		if key == "" {
			continue
		}

		// Solo setear si no existe ya en el entorno (el entorno tiene prioridad)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("leyendo .env (línea %d): %w", lineno, err)
	}

	return nil
}

// GetOrDefault retorna el valor de la variable de entorno key,
// o defaultVal si no está definida o está vacía.
func GetOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// GetBoolOrDefault retorna el valor booleano de la variable de entorno key.
// Acepta "true", "1", "yes" como verdadero (case-insensitive).
func GetBoolOrDefault(key string, defaultVal bool) bool {
	v := strings.ToLower(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	return v == "true" || v == "1" || v == "yes"
}

// GetIntOrDefault retorna el valor entero de la variable de entorno key,
// o defaultVal si no está definida, está vacía o no es un número válido.
func GetIntOrDefault(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	var result int
	if _, err := fmt.Sscanf(v, "%d", &result); err != nil {
		return defaultVal
	}
	return result
}