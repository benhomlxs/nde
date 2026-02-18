package hysteria2

import (
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"marznode/internal/models"
	"marznode/internal/storage"
)

type Config struct {
	raw     map[string]any
	inbound models.Inbound
}

func LoadConfig(raw string, apiPort, statsPort int, statsSecret string) (*Config, error) {
	content := raw
	if parsed, err := os.ReadFile(raw); err == nil {
		content = string(parsed)
	}

	cfg := map[string]any{}
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, err
	}

	cfg["auth"] = map[string]any{
		"type": "http",
		"http": map[string]any{"url": "http://127.0.0.1:" + strconv.Itoa(apiPort)},
	}
	cfg["trafficStats"] = map[string]any{
		"listen": "127.0.0.1:" + strconv.Itoa(statsPort),
		"secret": statsSecret,
	}

	port := 443
	if listen := toString(cfg["listen"]); listen != "" {
		parts := strings.Split(listen, ":")
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			port = n
		}
	}

	inbound := models.Inbound{
		Tag:      "hysteria2",
		Protocol: "hysteria2",
		Config: map[string]any{
			"tag":      "hysteria2",
			"protocol": "hysteria2",
			"port":     port,
			"tls":      "tls",
		},
	}

	obfs := toMap(cfg["obfs"])
	obfsType := toString(obfs["type"])
	if obfsType != "" {
		obfsCfg := toMap(obfs[obfsType])
		if pass := toString(obfsCfg["password"]); pass != "" {
			inbound.Config["header_type"] = obfsType
			inbound.Config["path"] = pass
		}
	}

	return &Config{
		raw:     cfg,
		inbound: inbound,
	}, nil
}

func (c *Config) RegisterInbounds(store storage.Storage) {
	store.RegisterInbound(c.inbound)
}

func (c *Config) Inbound() models.Inbound {
	return c.inbound
}

func (c *Config) Render() (string, error) {
	data, err := yaml.Marshal(c.raw)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
