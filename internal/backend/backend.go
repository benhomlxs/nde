package backend

import (
	"context"

	"marznode/internal/models"
)

type Backend interface {
	Name() string
	Type() string
	Version() string
	ConfigFormat() int32
	Running() bool

	ContainsTag(tag string) bool
	ListInbounds() []models.Inbound
	GetConfig(ctx context.Context) (string, error)

	Start(ctx context.Context, backendConfig string) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context, backendConfig string) error

	AddUser(ctx context.Context, user models.User, inbound models.Inbound) error
	RemoveUser(ctx context.Context, user models.User, inbound models.Inbound) error
	GetUsages(ctx context.Context, reset bool) (map[uint32]uint64, error)
	LogStream(ctx context.Context, includeBuffer bool) (<-chan string, error)
}
