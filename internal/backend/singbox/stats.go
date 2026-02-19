package singbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	sbstatspb "marznode/internal/gen/sbstatspb"
)

type StatsClient struct {
	address string

	mu   sync.Mutex
	conn *grpc.ClientConn
}

const statsCallTimeout = 8 * time.Second

func NewStatsClient(port int) *StatsClient {
	return &StatsClient{address: fmt.Sprintf("127.0.0.1:%d", port)}
}

func (c *StatsClient) connect(ctx context.Context) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return c.conn, nil
	}

	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dctx, c.address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

func (c *StatsClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
}

func (c *StatsClient) Close() {
	c.reset()
}

func (c *StatsClient) UserUsages(ctx context.Context, reset bool) (map[uint32]uint64, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}

	client := sbstatspb.NewStatsServiceClient(conn)
	rpcCtx, cancel := context.WithTimeout(ctx, statsCallTimeout)
	defer cancel()
	resp, err := client.QueryStats(rpcCtx, &sbstatspb.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  reset,
	})
	if err != nil {
		c.reset()
		return nil, err
	}

	out := make(map[uint32]uint64)
	for _, stat := range resp.Stat {
		parts := strings.Split(stat.Name, ">>>")
		if len(parts) < 2 {
			continue
		}
		ident := parts[1]
		dot := strings.IndexByte(ident, '.')
		if dot <= 0 {
			continue
		}
		var uid uint32
		_, _ = fmt.Sscanf(ident[:dot], "%d", &uid)
		out[uid] += uint64(stat.Value)
	}
	return out, nil
}

func (c *StatsClient) SysStats(ctx context.Context) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}

	client := sbstatspb.NewStatsServiceClient(conn)
	rpcCtx, cancel := context.WithTimeout(ctx, statsCallTimeout)
	defer cancel()
	_, err = client.GetSysStats(rpcCtx, &sbstatspb.SysStatsRequest{})
	if err != nil {
		c.reset()
		return err
	}
	return nil
}
