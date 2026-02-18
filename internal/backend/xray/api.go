package xray

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	proxymancommand "github.com/xtls/xray-core/app/proxyman/command"
	statscmd "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vmess"

	"marznode/internal/auth"
	"marznode/internal/config"
)

type API struct {
	address string

	mu   sync.Mutex
	conn *grpc.ClientConn
}

func NewAPI(port int) *API {
	return &API{
		address: fmt.Sprintf("127.0.0.1:%d", port),
	}
}

func (a *API) connect(ctx context.Context) (*grpc.ClientConn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.conn != nil {
		return a.conn, nil
	}

	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dctx, a.address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	a.conn = conn
	return conn, nil
}

func (a *API) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		_ = a.conn.Close()
	}
	a.conn = nil
}

func (a *API) AddUser(ctx context.Context, tag string, uid uint32, username, key, protocolName, flow string, algo config.AuthAlgorithm) error {
	conn, err := a.connect(ctx)
	if err != nil {
		return err
	}

	email := auth.UserIdentifier(uid, username)
	accountMsg, err := buildAccount(protocolName, key, flow, algo)
	if err != nil {
		return err
	}
	user := &protocol.User{
		Level:   0,
		Email:   email,
		Account: serial.ToTypedMessage(accountMsg),
	}

	client := proxymancommand.NewHandlerServiceClient(conn)
	_, err = client.AlterInbound(ctx, &proxymancommand.AlterInboundRequest{
		Tag:       tag,
		Operation: serial.ToTypedMessage(&proxymancommand.AddUserOperation{User: user}),
	})
	if err != nil {
		if isAlreadyExistsError(err) {
			return nil
		}
		a.reset()
		return err
	}
	return nil
}

func (a *API) RemoveUser(ctx context.Context, tag string, uid uint32, username string) error {
	conn, err := a.connect(ctx)
	if err != nil {
		return err
	}

	client := proxymancommand.NewHandlerServiceClient(conn)
	_, err = client.AlterInbound(ctx, &proxymancommand.AlterInboundRequest{
		Tag: tag,
		Operation: serial.ToTypedMessage(
			&proxymancommand.RemoveUserOperation{Email: auth.UserIdentifier(uid, username)},
		),
	})
	if err != nil {
		if isUserNotFoundError(err) {
			return nil
		}
		a.reset()
		return err
	}
	return nil
}

func (a *API) UserUsages(ctx context.Context, reset bool) (map[uint32]uint64, error) {
	conn, err := a.connect(ctx)
	if err != nil {
		return nil, err
	}

	client := statscmd.NewStatsServiceClient(conn)
	resp, err := client.QueryStats(ctx, &statscmd.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  reset,
	})
	if err != nil {
		a.reset()
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
		if dot < 0 {
			continue
		}
		var uid uint32
		_, _ = fmt.Sscanf(ident[:dot], "%d", &uid)
		out[uid] += uint64(stat.Value)
	}
	return out, nil
}

func buildAccount(protocolName, seed, flow string, algo config.AuthAlgorithm) (proto.Message, error) {
	switch strings.ToLower(protocolName) {
	case "vmess":
		id, err := auth.GenerateUUID(seed, algo)
		if err != nil {
			return nil, err
		}
		return &vmess.Account{Id: id}, nil
	case "vless":
		id, err := auth.GenerateUUID(seed, algo)
		if err != nil {
			return nil, err
		}
		return &vless.Account{Id: id, Flow: flow}, nil
	case "trojan":
		return &trojan.Account{Password: auth.GeneratePassword(seed, algo)}, nil
	case "shadowsocks":
		return &shadowsocks.Account{Password: auth.GeneratePassword(seed, algo)}, nil
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", protocolName)
	}
}

func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(status.Convert(err).Message())
	return strings.Contains(msg, "already exists")
}

func isUserNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(status.Convert(err).Message())
	return strings.Contains(msg, "user") && strings.Contains(msg, "not found")
}
