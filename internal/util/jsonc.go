package util

import (
	"encoding/json"
	"os"

	"github.com/tailscale/hujson"
)

func ParseJSONOrPath(raw string, out any) error {
	if err := parseJSON(raw, out); err == nil {
		return nil
	}

	data, err := os.ReadFile(raw)
	if err != nil {
		return err
	}
	return parseJSON(string(data), out)
}

func parseJSON(raw string, out any) error {
	std, err := hujson.Standardize([]byte(raw))
	if err != nil {
		return err
	}
	return json.Unmarshal(std, out)
}
