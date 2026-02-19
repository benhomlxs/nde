package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type AuthAlgorithm string

const (
	AuthPlain  AuthAlgorithm = "plain"
	AuthXXH128 AuthAlgorithm = "xxh128"
)

type Config struct {
	ServiceAddress string
	ServicePort    int
	Insecure       bool

	XrayEnabled                bool
	XrayExecutablePath         string
	XrayAssetsPath             string
	XrayConfigPath             string
	XrayVlessRealityFlow       string
	XrayRestartOnFailure       bool
	XrayRestartFailureInterval time.Duration
	XrayHealthCheckEnabled     bool
	XrayHealthCheckInterval    time.Duration
	XrayHealthCheckTimeout     time.Duration
	XrayHealthCheckFailures    int

	SingBoxEnabled                  bool
	SingBoxExecutablePath           string
	SingBoxConfigPath               string
	SingBoxRestartOnFailure         bool
	SingBoxRestartFailureInterval   time.Duration
	SingBoxUserModificationInterval time.Duration

	HysteriaEnabled        bool
	HysteriaExecutablePath string
	HysteriaConfigPath     string

	SSLCertFile       string
	SSLKeyFile        string
	SSLClientCertFile string

	Debug bool

	AuthGenerationAlgorithm AuthAlgorithm
}

func Load() Config {
	_ = godotenv.Load()

	return Config{
		ServiceAddress: getenv("SERVICE_ADDRESS", "0.0.0.0"),
		ServicePort:    getenvInt("SERVICE_PORT", 53042),
		Insecure:       getenvBool("INSECURE", false),

		XrayEnabled:                getenvBool("XRAY_ENABLED", true),
		XrayExecutablePath:         getenv("XRAY_EXECUTABLE_PATH", "/usr/bin/xray"),
		XrayAssetsPath:             getenv("XRAY_ASSETS_PATH", "/usr/share/xray"),
		XrayConfigPath:             getenv("XRAY_CONFIG_PATH", "/etc/xray/config.json"),
		XrayVlessRealityFlow:       getenv("XRAY_VLESS_REALITY_FLOW", "xtls-rprx-vision"),
		XrayRestartOnFailure:       getenvBool("XRAY_RESTART_ON_FAILURE", true),
		XrayRestartFailureInterval: time.Duration(getenvInt("XRAY_RESTART_ON_FAILURE_INTERVAL", 3)) * time.Second,
		XrayHealthCheckEnabled:     getenvBool("XRAY_HEALTH_CHECK_ENABLED", true),
		XrayHealthCheckInterval:    time.Duration(getenvInt("XRAY_HEALTH_CHECK_INTERVAL", 5)) * time.Second,
		XrayHealthCheckTimeout:     time.Duration(getenvInt("XRAY_HEALTH_CHECK_TIMEOUT", 2)) * time.Second,
		XrayHealthCheckFailures:    getenvInt("XRAY_HEALTH_CHECK_FAILURE_THRESHOLD", 3),

		SingBoxEnabled:                  getenvBool("SING_BOX_ENABLED", false),
		SingBoxExecutablePath:           getenv("SING_BOX_EXECUTABLE_PATH", "/usr/bin/sing-box"),
		SingBoxConfigPath:               getenv("SING_BOX_CONFIG_PATH", "/etc/sing-box/config.json"),
		SingBoxRestartOnFailure:         getenvBool("SING_BOX_RESTART_ON_FAILURE", false),
		SingBoxRestartFailureInterval:   time.Duration(getenvInt("SING_BOX_RESTART_ON_FAILURE_INTERVAL", 0)) * time.Second,
		SingBoxUserModificationInterval: time.Duration(getenvInt("SING_BOX_USER_MODIFICATION_INTERVAL", 30)) * time.Second,

		HysteriaEnabled:        getenvBool("HYSTERIA_ENABLED", false),
		HysteriaExecutablePath: getenv("HYSTERIA_EXECUTABLE_PATH", "/usr/bin/hysteria"),
		HysteriaConfigPath:     getenv("HYSTERIA_CONFIG_PATH", "/etc/hysteria/config.yaml"),

		SSLCertFile:       getenv("SSL_CERT_FILE", "./ssl_cert.pem"),
		SSLKeyFile:        getenv("SSL_KEY_FILE", "./ssl_key.pem"),
		SSLClientCertFile: getenv("SSL_CLIENT_CERT_FILE", ""),

		Debug: getenvBool("DEBUG", false),

		AuthGenerationAlgorithm: AuthAlgorithm(getenv("AUTH_GENERATION_ALGORITHM", string(AuthXXH128))),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
