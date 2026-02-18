package singbox

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"marznode/internal/auth"
	"marznode/internal/config"
	"marznode/internal/models"
	"marznode/internal/storage"
	"marznode/internal/util"
)

type Config struct {
	mu sync.Mutex

	Raw      map[string]any
	Inbounds []models.Inbound
	byTag    map[string]struct{}
}

func LoadConfig(ctx context.Context, raw string, apiPort int, xrayExecutable string) (*Config, error) {
	cfg := map[string]any{}
	if err := util.ParseJSONOrPath(raw, &cfg); err != nil {
		return nil, err
	}

	applyAPI(cfg, apiPort)
	inbounds := resolveInbounds(ctx, cfg, xrayExecutable)
	byTag := make(map[string]struct{}, len(inbounds))
	for _, inb := range inbounds {
		byTag[inb.Tag] = struct{}{}
	}

	return &Config{
		Raw:      cfg,
		Inbounds: inbounds,
		byTag:    byTag,
	}, nil
}

func (c *Config) ContainsTag(tag string) bool {
	_, ok := c.byTag[tag]
	return ok
}

func (c *Config) RegisterInbounds(store storage.Storage) {
	for _, inb := range c.Inbounds {
		store.RegisterInbound(inb)
	}
}

func (c *Config) JSON() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(c.Raw)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Config) AppendUser(user models.User, inbound models.Inbound, algo config.AuthAlgorithm) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	identifier := auth.UserIdentifier(user.ID, user.Username)
	inbounds, _ := c.Raw["inbounds"].([]any)
	for _, item := range inbounds {
		inb := toMap(item)
		if toString(inb["tag"]) != inbound.Tag {
			continue
		}

		users, _ := inb["users"].([]any)
		account, err := buildAccount(identifier, user.Key, inbound.Protocol, toString(inbound.Config["flow"]), algo)
		if err != nil {
			return err
		}
		users = append(users, account)
		inb["users"] = users
		break
	}

	exp := toMap(c.Raw["experimental"])
	v2api := toMap(exp["v2ray_api"])
	stats := toMap(v2api["stats"])
	userList, _ := stats["users"].([]any)
	for _, item := range userList {
		if toString(item) == identifier {
			c.Raw["experimental"] = exp
			return nil
		}
	}
	stats["users"] = append(userList, identifier)
	v2api["stats"] = stats
	exp["v2ray_api"] = v2api
	c.Raw["experimental"] = exp
	return nil
}

func (c *Config) RemoveUser(user models.User, inbound models.Inbound) {
	c.mu.Lock()
	defer c.mu.Unlock()

	identifier := auth.UserIdentifier(user.ID, user.Username)
	inbounds, _ := c.Raw["inbounds"].([]any)
	for _, item := range inbounds {
		inb := toMap(item)
		if toString(inb["tag"]) != inbound.Tag {
			continue
		}
		users, _ := inb["users"].([]any)
		next := make([]any, 0, len(users))
		for _, u := range users {
			m := toMap(u)
			if toString(m["name"]) == identifier || toString(m["username"]) == identifier {
				continue
			}
			next = append(next, u)
		}
		inb["users"] = next
		break
	}
}

func applyAPI(cfg map[string]any, apiPort int) {
	exp := toMap(cfg["experimental"])
	exp["v2ray_api"] = map[string]any{
		"listen": "127.0.0.1:" + toString(apiPort),
		"stats": map[string]any{
			"enabled": true,
			"users":   []string{},
		},
	}
	cfg["experimental"] = exp
}

func resolveInbounds(ctx context.Context, cfg map[string]any, xrayExecutable string) []models.Inbound {
	raw, _ := cfg["inbounds"].([]any)
	out := make([]models.Inbound, 0, len(raw))
	for _, item := range raw {
		inb := toMap(item)
		tag := toString(inb["tag"])
		protocol := toString(inb["type"])
		if tag == "" {
			continue
		}
		if _, ok := map[string]struct{}{
			"shadowsocks": {}, "vmess": {}, "trojan": {}, "vless": {}, "hysteria2": {}, "tuic": {}, "shadowtls": {},
		}[protocol]; !ok {
			continue
		}

		settings := map[string]any{
			"tag":         tag,
			"protocol":    protocol,
			"port":        toInt(inb["listen_port"]),
			"network":     nil,
			"tls":         "none",
			"sni":         []string{},
			"host":        []string{},
			"path":        nil,
			"header_type": nil,
			"flow":        nil,
		}

		tlsCfg := toMap(inb["tls"])
		if toBool(tlsCfg["enabled"]) {
			settings["tls"] = "tls"
			if sni := toString(tlsCfg["server_name"]); sni != "" {
				settings["sni"] = []string{sni}
			}
			reality := toMap(tlsCfg["reality"])
			if toBool(reality["enabled"]) {
				settings["tls"] = "reality"
				if pub := getX25519(ctx, xrayExecutable, toString(reality["private_key"])); pub != "" {
					settings["pbk"] = pub
				}
				sid := toStringSlice(reality["short_id"])
				if len(sid) > 0 {
					settings["sid"] = sid[0]
				}
			}
		}

		transport := toMap(inb["transport"])
		network := toString(transport["type"])
		if network != "" {
			settings["network"] = network
		}
		switch network {
		case "ws", "httpupgrade":
			settings["path"] = toString(transport["path"])
		case "http":
			settings["path"] = toString(transport["path"])
			settings["network"] = "tcp"
			settings["header_type"] = "http"
			settings["host"] = toStringSlice(transport["host"])
		case "grpc":
			settings["path"] = toString(transport["service_name"])
		}

		if protocol == "shadowtls" {
			settings["shadowtls_version"] = toString(inb["version"])
		}
		if protocol == "hysteria2" {
			obfs := toMap(inb["obfs"])
			settings["header_type"] = toString(obfs["type"])
			settings["path"] = toString(obfs["password"])
		}

		out = append(out, models.Inbound{
			Tag:      tag,
			Protocol: protocol,
			Config:   settings,
		})
	}
	return out
}

func getX25519(ctx context.Context, executable, privateKey string) string {
	if executable == "" || privateKey == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, executable, "x25519", "-i", privateKey).CombinedOutput()
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`(?m)^Password:\s*([^\n]+)$`).FindStringSubmatch(string(out))
	if len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func buildAccount(identifier, seed, protocol, flow string, algo config.AuthAlgorithm) (map[string]any, error) {
	switch protocol {
	case "vmess":
		u, err := auth.GenerateUUID(seed, algo)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": identifier, "uuid": u}, nil
	case "vless":
		u, err := auth.GenerateUUID(seed, algo)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": identifier, "uuid": u, "flow": flow}, nil
	case "tuic":
		u, err := auth.GenerateUUID(seed, algo)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": identifier, "uuid": u, "password": auth.GeneratePassword(seed, algo)}, nil
	case "naive", "socks", "mixed", "http":
		return map[string]any{"username": identifier, "password": auth.GeneratePassword(seed, algo)}, nil
	default:
		return map[string]any{"name": identifier, "password": auth.GeneratePassword(seed, algo)}, nil
	}
}

func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	default:
		return 0
	}
}

func toStringSlice(v any) []string {
	if arr, ok := v.([]string); ok {
		return arr
	}
	raw, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s := toString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true") || t == "1"
	default:
		return false
	}
}
