package xray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	configPath     string
	executablePath string
	assetsPath     string
	flowDefault    string
	authAlgo       config.AuthAlgorithm

	restartOnFailure bool
	restartInterval  time.Duration
	healthEnabled    bool
	healthInterval   time.Duration
	healthTimeout    time.Duration
	healthFailures   int
	healthCooldown   time.Duration

	store  storage.Storage
	runner *Runner

	mu         sync.RWMutex
	runtimeCfg *Config
	api        *API
	rawConfig  string

	restartMu       sync.Mutex
	stopPlanned     atomic.Bool
	lastStartTime   time.Time
	lastStartTimeMu sync.Mutex

	repopulateRunning atomic.Bool
	repopulateMu      sync.Mutex
	repopulateCancel  context.CancelFunc
	repopulateDone    chan struct{}
}

var _ backend.Backend = (*Backend)(nil)

func backendLogger() *slog.Logger {
	return slog.Default().With("component", "backend.xray")
}

func NewBackend(cfg config.Config, store storage.Storage) *Backend {
	b := &Backend{
		name:             "xray",
		configPath:       cfg.XrayConfigPath,
		executablePath:   cfg.XrayExecutablePath,
		assetsPath:       cfg.XrayAssetsPath,
		flowDefault:      cfg.XrayVlessRealityFlow,
		authAlgo:         cfg.AuthGenerationAlgorithm,
		restartOnFailure: cfg.XrayRestartOnFailure,
		restartInterval:  cfg.XrayRestartFailureInterval,
		healthEnabled:    cfg.XrayHealthCheckEnabled,
		healthInterval:   cfg.XrayHealthCheckInterval,
		healthTimeout:    cfg.XrayHealthCheckTimeout,
		healthFailures:   cfg.XrayHealthCheckFailures,
		healthCooldown:   cfg.XrayHealthRestartCooldown,
		store:            store,
		runner:           NewRunner(cfg.XrayExecutablePath, cfg.XrayAssetsPath),
	}
	go b.monitorFailures(context.Background())
	go b.monitorHealth(context.Background())
	return b
}

func (b *Backend) Name() string { return b.name }
func (b *Backend) Type() string { return "xray" }
func (b *Backend) Version() string {
	return b.runner.Version(context.Background())
}
func (b *Backend) ConfigFormat() int32 { return 1 } // JSON
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
		data, err := os.ReadFile(b.configPath)
		if err != nil {
			return err
		}
		backendConfig = string(data)
	} else {
		if err := b.saveConfig(backendConfig); err != nil {
			return err
		}
	}

	apiPort, err := util.FindFreePort()
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(ctx, backendConfig, apiPort, b.executablePath, b.flowDefault)
	if err != nil {
		return err
	}
	jsonConfig, err := cfg.JSON()
	if err != nil {
		return err
	}
	cfg.RegisterInbounds(b.store)

	if err := b.runner.Start(ctx, jsonConfig); err != nil {
		return err
	}

	b.mu.Lock()
	b.runtimeCfg = cfg
	b.api = NewAPI(apiPort)
	b.rawConfig = backendConfig
	b.mu.Unlock()

	if err := b.waitAPIReady(); err != nil {
		_ = b.Stop(context.Background())
		return fmt.Errorf("xray api not ready after start: %w", err)
	}

	b.lastStartTimeMu.Lock()
	b.lastStartTime = time.Now()
	b.lastStartTimeMu.Unlock()

	b.startRepopulateStorageUsers()
	return nil
}

func (b *Backend) waitAPIReady() error {
	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return util.Retry(readyCtx, 6, 300*time.Millisecond, func() error {
		b.mu.RLock()
		api := b.api
		b.mu.RUnlock()
		if api == nil {
			return errors.New("xray api unavailable")
		}

		probeCtx, probeCancel := context.WithTimeout(readyCtx, 1200*time.Millisecond)
		defer probeCancel()
		return api.SysStats(probeCtx)
	})
}

func (b *Backend) startRepopulateStorageUsers() {
	b.repopulateMu.Lock()
	if b.repopulateRunning.Load() {
		b.repopulateMu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	done := make(chan struct{})
	b.repopulateCancel = cancel
	b.repopulateDone = done
	b.repopulateRunning.Store(true)
	b.repopulateMu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			b.repopulateMu.Lock()
			b.repopulateCancel = nil
			b.repopulateDone = nil
			b.repopulateRunning.Store(false)
			b.repopulateMu.Unlock()
		}()
		defer cancel()

		err := util.Retry(ctx, 6, 400*time.Millisecond, func() error {
			return b.repopulateStorageUsers(ctx)
		})
		if err != nil {
			if ctx.Err() != nil {
				backendLogger().Info("background user repopulate canceled", "reason", ctx.Err())
				return
			}
			backendLogger().Warn("background user repopulate finished with errors", "error", err)
			return
		}
		backendLogger().Info("background user repopulate completed")
	}()
}

func (b *Backend) stopRepopulateStorageUsers(waitTimeout time.Duration) {
	b.repopulateMu.Lock()
	cancel := b.repopulateCancel
	done := b.repopulateDone
	b.repopulateMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}

	if waitTimeout <= 0 {
		<-done
		return
	}

	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		backendLogger().Warn("timeout waiting for repopulate worker shutdown", "timeout", waitTimeout)
	}
}

func (b *Backend) repopulateStorageUsers(ctx context.Context) error {
	inbounds := b.ListInbounds()
	var errs []error
	for _, inb := range inbounds {
		users := b.store.ListInboundUsers(inb.Tag)
		for _, user := range users {
			if err := b.AddUser(ctx, user, inb); err != nil {
				errs = append(errs, fmt.Errorf("uid=%d tag=%s: %w", user.ID, inb.Tag, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (b *Backend) saveConfig(raw string) error {
	var pretty bytesMap
	if err := util.ParseJSONOrPath(raw, &pretty); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.configPath, data, 0o644)
}

type bytesMap map[string]any

func (b *Backend) Stop(ctx context.Context) error {
	b.stopPlanned.Store(true)
	defer b.stopPlanned.Store(false)

	waitTimeout := 20 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < waitTimeout {
			waitTimeout = remaining
		}
	}
	b.stopRepopulateStorageUsers(waitTimeout)

	err := b.runner.Stop(ctx)

	b.mu.Lock()
	api := b.api
	if b.runtimeCfg != nil {
		for _, inb := range b.runtimeCfg.Inbounds {
			b.store.RemoveInbound(inb.Tag)
		}
	}
	b.runtimeCfg = nil
	b.api = nil
	b.mu.Unlock()

	if api != nil {
		api.Close()
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
	b.mu.RLock()
	api := b.api
	b.mu.RUnlock()
	if api == nil {
		return errors.New("xray api unavailable")
	}

	return util.Retry(ctx, 5, 200*time.Millisecond, func() error {
		return api.AddUser(
			ctx,
			inbound.Tag,
			user.ID,
			user.Username,
			user.Key,
			inbound.Protocol,
			toString(inbound.Config["flow"]),
			toString(inbound.Config["method"]),
			b.authAlgo,
		)
	})
}

func (b *Backend) RemoveUser(ctx context.Context, user models.User, inbound models.Inbound) error {
	b.mu.RLock()
	api := b.api
	b.mu.RUnlock()
	if api == nil {
		return errors.New("xray api unavailable")
	}

	return util.Retry(ctx, 5, 200*time.Millisecond, func() error {
		return api.RemoveUser(ctx, inbound.Tag, user.ID, user.Username)
	})
}

func (b *Backend) GetUsages(ctx context.Context, reset bool) (map[uint32]uint64, error) {
	b.mu.RLock()
	api := b.api
	b.mu.RUnlock()
	if api == nil {
		return map[uint32]uint64{}, nil
	}
	usages, err := api.UserUsages(ctx, reset)
	if err != nil {
		return nil, err
	}
	return usages, nil
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
			api := b.api
			raw := b.rawConfig
			b.mu.RUnlock()
			if api == nil {
				consecutiveFailures = 0
				continue
			}

			hCtx, cancel := context.WithTimeout(ctx, timeout)
			err := api.SysStats(hCtx)
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

			if b.healthCooldown > 0 {
				b.lastStartTimeMu.Lock()
				lastStart := b.lastStartTime
				b.lastStartTimeMu.Unlock()
				if !lastStart.IsZero() {
					if elapsed := time.Since(lastStart); elapsed < b.healthCooldown {
						backendLogger().Warn("health restart skipped: within cooldown period", "elapsed", elapsed, "cooldown", b.healthCooldown)
						consecutiveFailures = 0
						continue
					}
				}
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
