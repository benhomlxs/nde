package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"marznode/internal/models"
	"marznode/internal/storage"
	"marznode/internal/util"
)

type Config struct {
	Raw        map[string]any
	Inbounds   []models.Inbound
	inboundTag map[string]struct{}
}

var transportMap = map[string]string{
	"tcp":         "tcp",
	"raw":         "tcp",
	"splithttp":   "splithttp",
	"xhttp":       "splithttp",
	"grpc":        "grpc",
	"kcp":         "kcp",
	"mkcp":        "kcp",
	"h2":          "http",
	"h3":          "http",
	"http":        "http",
	"ws":          "ws",
	"websocket":   "ws",
	"httpupgrade": "httpupgrade",
	"quic":        "quic",
}

func LoadConfig(ctx context.Context, raw string, apiPort int, executablePath, realityFlow string) (*Config, error) {
	cfg := map[string]any{}
	if err := util.ParseJSONOrPath(raw, &cfg); err != nil {
		return nil, err
	}

	applyAPIConfig(cfg, apiPort)
	inbounds := resolveInbounds(ctx, cfg, executablePath, realityFlow)

	tagSet := make(map[string]struct{}, len(inbounds))
	for _, inb := range inbounds {
		tagSet[inb.Tag] = struct{}{}
	}

	return &Config{
		Raw:        cfg,
		Inbounds:   inbounds,
		inboundTag: tagSet,
	}, nil
}

func (c *Config) ContainsTag(tag string) bool {
	_, ok := c.inboundTag[tag]
	return ok
}

func (c *Config) RegisterInbounds(store storage.Storage) {
	for _, inb := range c.Inbounds {
		store.RegisterInbound(inb)
	}
}

func (c *Config) JSON() (string, error) {
	b, err := json.Marshal(c.Raw)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func applyAPIConfig(cfg map[string]any, apiPort int) {
	cfg["api"] = map[string]any{
		"services": []string{"HandlerService", "StatsService", "LoggerService"},
		"tag":      "API",
	}
	cfg["stats"] = map[string]any{}

	policyForced := map[string]any{
		"levels": map[string]any{
			"0": map[string]any{
				"statsUserUplink":   true,
				"statsUserDownlink": true,
			},
		},
		"system": map[string]any{
			"statsInboundDownlink":  false,
			"statsInboundUplink":    false,
			"statsOutboundDownlink": true,
			"statsOutboundUplink":   true,
		},
	}
	if policy, ok := cfg["policy"].(map[string]any); ok {
		cfg["policy"] = mergeMap(policy, policyForced)
	} else {
		cfg["policy"] = policyForced
	}

	apiInbound := map[string]any{
		"listen":   "127.0.0.1",
		"port":     apiPort,
		"protocol": "dokodemo-door",
		"settings": map[string]any{
			"address": "127.0.0.1",
		},
		"tag": "API_INBOUND",
	}

	inbounds, _ := cfg["inbounds"].([]any)
	cfg["inbounds"] = append([]any{apiInbound}, inbounds...)

	rule := map[string]any{
		"inboundTag":  []string{"API_INBOUND"},
		"outboundTag": "API",
		"type":        "field",
	}
	routing, ok := cfg["routing"].(map[string]any)
	if !ok {
		routing = map[string]any{"rules": []any{}}
		cfg["routing"] = routing
	}
	rules, _ := routing["rules"].([]any)
	routing["rules"] = append([]any{rule}, rules...)
}

func mergeMap(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		vMap, vIsMap := v.(map[string]any)
		oMap, oIsMap := out[k].(map[string]any)
		if vIsMap && oIsMap {
			out[k] = mergeMap(oMap, vMap)
			continue
		}
		out[k] = v
	}
	return out
}

func resolveInbounds(ctx context.Context, cfg map[string]any, executablePath, realityFlow string) []models.Inbound {
	inboundsAny, _ := cfg["inbounds"].([]any)
	out := make([]models.Inbound, 0, len(inboundsAny))

	for _, item := range inboundsAny {
		inb, ok := item.(map[string]any)
		if !ok {
			continue
		}
		protocol := strings.ToLower(toString(inb["protocol"]))
		if _, allowed := map[string]struct{}{
			"vmess": {}, "trojan": {}, "vless": {}, "shadowsocks": {}, "shadowsocks2022": {},
		}[protocol]; !allowed {
			continue
		}
		tag := toString(inb["tag"])
		if tag == "" {
			continue
		}

		settings := map[string]any{
			"tag":         tag,
			"protocol":    protocol,
			"port":        toInt(inb["port"]),
			"network":     "tcp",
			"tls":         "none",
			"sni":         []string{},
			"host":        []string{},
			"path":        nil,
			"header_type": nil,
			"flow":        nil,
			"method":      nil,
		}

		stream := toMap(inb["streamSettings"])
		netName := strings.ToLower(toString(stream["network"]))
		if mapped, ok := transportMap[netName]; ok {
			settings["network"] = mapped
		}

		security := strings.ToLower(toString(stream["security"]))
		tlsSettings := toMap(stream[security+"Settings"])
		switch security {
		case "tls":
			settings["tls"] = "tls"
		case "reality":
			settings["tls"] = "reality"
			settings["fp"] = "chrome"
			settings["sni"] = toStringSlice(tlsSettings["serverNames"])
			if protocol == "vless" && settings["network"] == "tcp" {
				settings["flow"] = realityFlow
			}
			privateKey := toString(tlsSettings["privateKey"])
			if privateKey != "" {
				if pub, err := getX25519PublicKey(ctx, executablePath, privateKey); err == nil {
					settings["pbk"] = pub
				}
			}
			shortIDs := toStringSlice(tlsSettings["shortIds"])
			if len(shortIDs) > 0 {
				settings["sid"] = shortIDs[0]
			} else {
				settings["sid"] = ""
			}
		}

		netSettings := toMap(stream[netName+"Settings"])
		switch netName {
		case "tcp", "raw":
			header := toMap(netSettings["header"])
			settings["header_type"] = toString(header["type"])
			request := toMap(header["request"])
			path := toStringSlice(request["path"])
			if len(path) > 0 {
				settings["path"] = path[0]
			}
			settings["host"] = toStringSlice(toMap(request["headers"])["Host"])
		case "ws", "websocket", "httpupgrade", "splithttp", "xhttp":
			settings["path"] = toString(netSettings["path"])
			settings["host"] = toStringSlice(netSettings["host"])
		case "grpc":
			settings["path"] = toString(netSettings["serviceName"])
			if v, ok := netSettings["multiMode"]; ok {
				settings["multiMode"] = v
			} else {
				settings["multiMode"] = false
			}
		case "kcp", "mkcp":
			settings["path"] = toString(netSettings["seed"])
			settings["header_type"] = toString(toMap(netSettings["header"])["type"])
		case "quic":
			settings["host"] = []string{toString(netSettings["security"])}
			settings["path"] = toString(netSettings["key"])
			settings["header_type"] = toString(toMap(netSettings["header"])["type"])
		case "http":
			settings["path"] = toString(netSettings["path"])
			settings["host"] = toStringSlice(netSettings["host"])
		}

		if protocol == "shadowsocks" || protocol == "shadowsocks2022" {
			ssSettings := toMap(inb["settings"])
			method := strings.ToLower(toString(ssSettings["method"]))
			if method == "" {
				if clients, ok := ssSettings["clients"].([]any); ok && len(clients) > 0 {
					method = strings.ToLower(toString(toMap(clients[0])["method"]))
				}
			}
			if method != "" {
				settings["method"] = method
			}
			settings["network"] = nil
		}

		out = append(out, models.Inbound{
			Tag:      tag,
			Protocol: protocol,
			Config:   settings,
		})
	}

	return out
}

func getX25519PublicKey(ctx context.Context, executablePath, privateKey string) (string, error) {
	args := []string{"x25519"}
	if privateKey != "" {
		args = append(args, "-i", privateKey)
	}
	out, err := exec.CommandContext(ctx, executablePath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("x25519 failed: %w", err)
	}
	raw := string(out)

	oldStyle := regexp.MustCompile(`(?s)Private key:\s*([^\n]+)\nPublic key:\s*([^\n]+)`)
	if match := oldStyle.FindStringSubmatch(raw); len(match) == 3 {
		return strings.TrimSpace(match[2]), nil
	}
	newStyle := regexp.MustCompile(`(?m)^Password:\s*([^\n]+)$`)
	if match := newStyle.FindStringSubmatch(raw); len(match) == 2 {
		return strings.TrimSpace(match[1]), nil
	}
	return "", fmt.Errorf("unable to parse x25519 output")
}

func toMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
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
	raw, ok := v.([]any)
	if !ok {
		if cast, ok := v.([]string); ok {
			return cast
		}
		return []string{}
	}

	out := make([]string, 0, len(raw))
	for _, i := range raw {
		if s := toString(i); s != "" {
			out = append(out, s)
		}
	}
	return out
}
