package xray

import (
	"context"
	"encoding/json"
	"errors"
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

	store  storage.Storage
	runner *Runner

	mu         sync.RWMutex
	runtimeCfg *Config
	api        *API
	rawConfig  string

	restartMu   sync.Mutex
	stopPlanned atomic.Bool
}

var _ backend.Backend = (*Backend)(nil)

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
		store:            store,
		runner:           NewRunner(cfg.XrayExecutablePath, cfg.XrayAssetsPath),
	}
	go b.monitorFailures(context.Background())
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

	_ = util.Retry(ctx, 6, 300*time.Millisecond, func() error {
		return b.repopulateStorageUsers(ctx)
	})
	return nil
}

func (b *Backend) repopulateStorageUsers(ctx context.Context) error {
	inbounds := b.ListInbounds()
	for _, inb := range inbounds {
		users := b.store.ListInboundUsers(inb.Tag)
		for _, user := range users {
			if err := b.AddUser(ctx, user, inb); err != nil {
				return err
			}
		}
	}
	return nil
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

	err := b.runner.Stop(ctx)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runtimeCfg != nil {
		for _, inb := range b.runtimeCfg.Inbounds {
			b.store.RemoveInbound(inb.Tag)
		}
	}
	b.runtimeCfg = nil
	b.api = nil
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
		return api.AddUser(ctx, inbound.Tag, user.ID, user.Username, user.Key, inbound.Protocol, toString(inbound.Config["flow"]), b.authAlgo)
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
			_ = b.Restart(context.Background(), raw)
		}
	}
}
