package prft

import (
	"encoding/json"
	"os"

	"gopkg.in/yaml.v2"
)

func encode(v interface{}, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	default:
		return yaml.NewEncoder(os.Stdout).Encode(v)
	}
}
