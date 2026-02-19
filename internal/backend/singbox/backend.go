package singbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

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
	fullConfigPath string
	xrayExecutable string
	authAlgo       config.AuthAlgorithm

	restartOnFailure bool
	restartInterval  time.Duration
	userUpdateTick   time.Duration
	healthEnabled    bool
	healthInterval   time.Duration
	healthTimeout    time.Duration
	healthFailures   int

	store  storage.Storage
	runner *Runner

	mu         sync.RWMutex
	runtimeCfg *Config
	stats      *StatsClient
	rawConfig  string

	restartMu   sync.Mutex
	configMu    sync.Mutex
	configDirty atomic.Bool
	stopPlanned atomic.Bool
}

var _ backend.Backend = (*Backend)(nil)

func backendLogger() *slog.Logger {
	return slog.Default().With("component", "backend.sing-box")
}

func NewBackend(cfg config.Config, store storage.Storage) *Backend {
	userUpdateTick := cfg.SingBoxUserModificationInterval
	if userUpdateTick <= 0 {
		userUpdateTick = 30 * time.Second
	}

	b := &Backend{
		name:             "sing-box",
		executablePath:   cfg.SingBoxExecutablePath,
		configPath:       cfg.SingBoxConfigPath,
		fullConfigPath:   cfg.SingBoxConfigPath + ".full",
		xrayExecutable:   cfg.XrayExecutablePath,
		authAlgo:         cfg.AuthGenerationAlgorithm,
		restartOnFailure: cfg.SingBoxRestartOnFailure,
		restartInterval:  cfg.SingBoxRestartFailureInterval,
		userUpdateTick:   userUpdateTick,
		healthEnabled:    cfg.SingBoxHealthCheckEnabled,
		healthInterval:   cfg.SingBoxHealthCheckInterval,
		healthTimeout:    cfg.SingBoxHealthCheckTimeout,
		healthFailures:   cfg.SingBoxHealthCheckFailures,
		store:            store,
		runner:           NewRunner(cfg.SingBoxExecutablePath),
	}
	go b.monitorFailures(context.Background())
	go b.userUpdateWorker(context.Background())
	go b.monitorHealth(context.Background())
	return b
}

func (b *Backend) Name() string { return b.name }
func (b *Backend) Type() string { return "sing-box" }
func (b *Backend) Version() string {
	return b.runner.Version(context.Background())
}
func (b *Backend) ConfigFormat() int32 { return 1 }
func (b *Backend) Running() bool       { return b.runner.Running() }

func (b *Backend) ContainsTag(tag string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.runtimeCfg == nil {
		return false
	}
	return b.runtimeCfg.ContainsTag(tag)
}

func (b *Backend) ListInbounds() []models.Inbound {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.runtimeCfg == nil {
		return nil
	}
	out := make([]models.Inbound, len(b.runtimeCfg.Inbounds))
	copy(out, b.runtimeCfg.Inbounds)
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
		if err := b.saveConfig(b.configPath, backendConfig); err != nil {
			return err
		}
	}

	apiPort, err := util.FindFreePort()
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(ctx, backendConfig, apiPort, b.xrayExecutable)
	if err != nil {
		return err
	}
	cfg.RegisterInbounds(b.store)
	if err := b.addStorageUsers(cfg); err != nil {
		return err
	}

	jsonCfg, err := cfg.JSON()
	if err != nil {
		return err
	}
	if err := b.saveConfig(b.fullConfigPath, jsonCfg); err != nil {
		return err
	}
	if err := b.runner.Start(ctx, b.fullConfigPath); err != nil {
		return err
	}

	b.mu.Lock()
	b.runtimeCfg = cfg
	b.stats = NewStatsClient(apiPort)
	b.rawConfig = backendConfig
	b.mu.Unlock()
	return nil
}

func (b *Backend) addStorageUsers(cfg *Config) error {
	for _, inb := range cfg.Inbounds {
		for _, user := range b.store.ListInboundUsers(inb.Tag) {
			if err := cfg.AppendUser(user, inb, b.authAlgo); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) saveConfig(path, raw string) error {
	var parsed map[string]any
	if err := util.ParseJSONOrPath(raw, &parsed); err != nil {
		return err
	}
	data, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (b *Backend) Stop(ctx context.Context) error {
	b.stopPlanned.Store(true)
	defer b.stopPlanned.Store(false)

	err := b.runner.Stop(ctx)

	b.mu.Lock()
	stats := b.stats
	if b.runtimeCfg != nil {
		for _, inb := range b.runtimeCfg.Inbounds {
			b.store.RemoveInbound(inb.Tag)
		}
	}
	b.runtimeCfg = nil
	b.stats = nil
	b.mu.Unlock()

	if stats != nil {
		stats.Close()
	}
	return err
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
	b.configMu.Lock()
	defer b.configMu.Unlock()

	b.mu.RLock()
	cfg := b.runtimeCfg
	b.mu.RUnlock()
	if cfg == nil {
		return errors.New("sing-box config unavailable")
	}
	if err := cfg.AppendUser(user, inbound, b.authAlgo); err != nil {
		return err
	}
	b.configDirty.Store(true)
	return nil
}

func (b *Backend) RemoveUser(ctx context.Context, user models.User, inbound models.Inbound) error {
	_ = ctx
	b.configMu.Lock()
	defer b.configMu.Unlock()

	b.mu.RLock()
	cfg := b.runtimeCfg
	b.mu.RUnlock()
	if cfg == nil {
		return errors.New("sing-box config unavailable")
	}
	cfg.RemoveUser(user, inbound)
	b.configDirty.Store(true)
	return nil
}

func (b *Backend) GetUsages(ctx context.Context, reset bool) (map[uint32]uint64, error) {
	b.mu.RLock()
	stats := b.stats
	b.mu.RUnlock()
	if stats == nil {
		return map[uint32]uint64{}, nil
	}
	return stats.UserUsages(ctx, reset)
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

func (b *Backend) userUpdateWorker(ctx context.Context) {
	ticker := time.NewTicker(b.userUpdateTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !b.configDirty.Load() {
				continue
			}

			b.configMu.Lock()
			b.mu.RLock()
			cfg := b.runtimeCfg
			b.mu.RUnlock()
			if cfg == nil {
				b.configMu.Unlock()
				continue
			}
			raw, err := cfg.JSON()
			if err == nil {
				err = b.saveConfig(b.fullConfigPath, raw)
			}
			if err == nil {
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = b.runner.Reload(rctx)
				cancel()
			}
			if err == nil {
				b.configDirty.Store(false)
			}
			b.configMu.Unlock()
		}
	}
}

func (b *Backend) monitorFailures(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.runner.ExitChannel():
			if b.stopPlanned.Load() || !b.restartOnFailure {
				continue
			}
			time.Sleep(b.restartInterval)
			b.mu.RLock()
			raw := b.rawConfig
			b.mu.RUnlock()
			if err := b.Restart(context.Background(), raw); err != nil {
				backendLogger().Error("restart on process exit failed", "error", err)
			}
		}
	}
}

func (b *Backend) monitorHealth(ctx context.Context) {
	if !b.healthEnabled {
		return
	}

	interval := b.healthInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	timeout := b.healthTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	maxFailures := b.healthFailures
	if maxFailures <= 0 {
		maxFailures = 3
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if b.stopPlanned.Load() || !b.Running() {
				consecutiveFailures = 0
				continue
			}

			b.mu.RLock()
			stats := b.stats
			raw := b.rawConfig
			b.mu.RUnlock()
			if stats == nil {
				consecutiveFailures = 0
				continue
			}

			hCtx, cancel := context.WithTimeout(ctx, timeout)
			err := stats.SysStats(hCtx)
			cancel()
			if err == nil {
				consecutiveFailures = 0
				continue
			}

			consecutiveFailures++
			backendLogger().Warn("health check failed", "failure_count", consecutiveFailures, "failure_threshold", maxFailures, "error", err)
			if consecutiveFailures < maxFailures {
				continue
			}

			consecutiveFailures = 0
			backendLogger().Warn("backend unhealthy, attempting restart")
			if err := b.Restart(context.Background(), raw); err != nil {
				backendLogger().Error("health restart failed", "error", err)
				continue
			}
			backendLogger().Info("health restart succeeded")
		}
	}
}
