package hysteria2

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"marznode/internal/auth"
	"marznode/internal/backend"
	"marznode/internal/config"
	"marznode/internal/models"
	"marznode/internal/storage"
	"marznode/internal/util"
)

type Backend struct {
	name           string
	executablePath string
	configPath     string
	authAlgo       config.AuthAlgorithm

	store  storage.Storage
	runner *Runner

	mu          sync.RWMutex
	usersByPass map[string]models.User
	inbounds    []models.Inbound
	server      *http.Server
	statsPort   int
	statsSecret string
	rawConfig   string

	restartMu sync.Mutex
}

var _ backend.Backend = (*Backend)(nil)

func NewBackend(cfg config.Config, store storage.Storage) *Backend {
	return &Backend{
		name:           "hysteria2",
		executablePath: cfg.HysteriaExecutablePath,
		configPath:     cfg.HysteriaConfigPath,
		authAlgo:       cfg.AuthGenerationAlgorithm,
		store:          store,
		runner:         NewRunner(cfg.HysteriaExecutablePath),
		usersByPass:    make(map[string]models.User),
	}
}

func (b *Backend) Name() string { return b.name }
func (b *Backend) Type() string { return "hysteria2" }
func (b *Backend) Version() string {
	return b.runner.Version(context.Background())
}
func (b *Backend) ConfigFormat() int32 { return 2 }
func (b *Backend) Running() bool       { return b.runner.Running() }

func (b *Backend) ContainsTag(tag string) bool {
	return tag == "hysteria2"
}

func (b *Backend) ListInbounds() []models.Inbound {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]models.Inbound, len(b.inbounds))
	copy(out, b.inbounds)
	return out
}

func (b *Backend) GetConfig(ctx context.Context) (string, error) {
	_ = ctx
	data, err := os.ReadFile(b.configPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (b *Backend) Start(ctx context.Context, backendConfig string) error {
	if backendConfig == "" {
		raw, err := os.ReadFile(b.configPath)
		if err != nil {
			return err
		}
		backendConfig = string(raw)
	} else {
		if err := os.WriteFile(b.configPath, []byte(backendConfig), 0o644); err != nil {
			return err
		}
	}

	authPort, err := util.FindFreePort()
	if err != nil {
		return err
	}
	statsPort, err := util.FindFreePort()
	if err != nil {
		return err
	}
	secret, err := randomHex(16)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig(backendConfig, authPort, statsPort, secret)
	if err != nil {
		return err
	}
	cfg.RegisterInbounds(b.store)
	rendered, err := cfg.Render()
	if err != nil {
		return err
	}

	if err := b.startAuthServer(authPort); err != nil {
		return err
	}
	if err := b.runner.Start(ctx, rendered); err != nil {
		_ = b.stopAuthServer(context.Background())
		return err
	}

	b.mu.Lock()
	b.statsPort = statsPort
	b.statsSecret = secret
	b.inbounds = []models.Inbound{cfg.Inbound()}
	b.rawConfig = backendConfig
	b.mu.Unlock()
	return nil
}

func (b *Backend) startAuthServer(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", b.authHandler)
	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}
	b.mu.Lock()
	b.server = server
	b.mu.Unlock()

	go func() {
		_ = server.ListenAndServe()
	}()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (b *Backend) stopAuthServer(ctx context.Context) error {
	b.mu.RLock()
	server := b.server
	b.mu.RUnlock()
	if server == nil {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return server.Shutdown(sctx)
}

func (b *Backend) Stop(ctx context.Context) error {
	_ = b.stopAuthServer(ctx)
	b.store.RemoveInbound("hysteria2")
	return b.runner.Stop(ctx)
}

func (b *Backend) Restart(ctx context.Context, backendConfig string) error {
	b.restartMu.Lock()
	defer b.restartMu.Unlock()
	if err := b.Stop(ctx); err != nil {
		return err
	}
	return b.Start(ctx, backendConfig)
}

func (b *Backend) AddUser(ctx context.Context, user models.User, inbound models.Inbound) error {
	_ = ctx
	_ = inbound
	pass := auth.GeneratePassword(user.Key, b.authAlgo)
	b.mu.Lock()
	b.usersByPass[pass] = user
	b.mu.Unlock()
	return nil
}

func (b *Backend) RemoveUser(ctx context.Context, user models.User, inbound models.Inbound) error {
	_ = inbound
	pass := auth.GeneratePassword(user.Key, b.authAlgo)
	b.mu.Lock()
	delete(b.usersByPass, pass)
	statsPort := b.statsPort
	secret := b.statsSecret
	b.mu.Unlock()

	body, _ := json.Marshal([]string{auth.UserIdentifier(user.ID, user.Username)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/kick", statsPort), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (b *Backend) GetUsages(ctx context.Context, reset bool) (map[uint32]uint64, error) {
	_ = reset
	b.mu.RLock()
	statsPort := b.statsPort
	secret := b.statsSecret
	b.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/traffic?clear=1", statsPort), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return map[uint32]uint64{}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	raw := map[string]map[string]uint64{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make(map[uint32]uint64, len(raw))
	for ident, usage := range raw {
		var uid uint32
		_, _ = fmt.Sscanf(strings.SplitN(ident, ".", 2)[0], "%d", &uid)
		out[uid] = usage["tx"] + usage["rx"]
	}
	return out, nil
}

func (b *Backend) LogStream(ctx context.Context, includeBuffer bool) (<-chan string, error) {
	src := b.runner.Logs(includeBuffer)
	out := make(chan string, 256)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-src:
				if !ok {
					return
				}
				out <- line
			}
		}
	}()
	return out, nil
}

func (b *Backend) authHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	req := map[string]string{}
	_ = json.Unmarshal(body, &req)
	pass := req["auth"]

	b.mu.RLock()
	user, ok := b.usersByPass[pass]
	b.mu.RUnlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	resp := map[string]any{
		"ok": true,
		"id": auth.UserIdentifier(user.ID, user.Username),
	}
	data, _ := json.Marshal(resp)
	_, _ = w.Write(data)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
