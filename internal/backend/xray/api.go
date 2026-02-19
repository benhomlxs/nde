package xray

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

const apiCallTimeout = 8 * time.Second

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

func (a *API) Close() {
	a.reset()
}

func (a *API) AddUser(
	ctx context.Context,
	tag string,
	uid uint32,
	username, key, protocolName, flow, method string,
	algo config.AuthAlgorithm,
) error {
	conn, err := a.connect(ctx)
	if err != nil {
		return err
	}

	email := auth.UserIdentifier(uid, username)
	accountMsg, err := buildAccount(protocolName, key, flow, method, algo)
	if err != nil {
		return err
	}
	user := &protocol.User{
		Level:   0,
		Email:   email,
		Account: serial.ToTypedMessage(accountMsg),
	}

	client := proxymancommand.NewHandlerServiceClient(conn)
	rpcCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	_, err = client.AlterInbound(rpcCtx, &proxymancommand.AlterInboundRequest{
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
	rpcCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	_, err = client.AlterInbound(rpcCtx, &proxymancommand.AlterInboundRequest{
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
	rpcCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	resp, err := client.QueryStats(rpcCtx, &statscmd.QueryStatsRequest{
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

func (a *API) SysStats(ctx context.Context) error {
	conn, err := a.connect(ctx)
	if err != nil {
		return err
	}

	client := statscmd.NewStatsServiceClient(conn)
	rpcCtx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	_, err = client.GetSysStats(rpcCtx, &statscmd.SysStatsRequest{})
	if err != nil {
		a.reset()
		return err
	}
	return nil
}

func buildAccount(protocolName, seed, flow, method string, algo config.AuthAlgorithm) (proto.Message, error) {
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
		password := auth.GeneratePassword(seed, algo)
		method = strings.ToLower(method)
		if strings.HasPrefix(method, "2022-blake3") {
			password = ensureBase64Password(password, method)
			return &shadowsocks.Account{Password: password}, nil
		}
		return &shadowsocks.Account{
			Password:   password,
			CipherType: cipherTypeFromMethod(method),
		}, nil
	case "shadowsocks2022":
		password := ensureBase64Password(auth.GeneratePassword(seed, algo), method)
		return &shadowsocks.Account{Password: password}, nil
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", protocolName)
	}
}

func cipherTypeFromMethod(method string) shadowsocks.CipherType {
	switch strings.ToLower(method) {
	case "aes-128-gcm", "aes_128_gcm":
		return shadowsocks.CipherType_AES_128_GCM
	case "aes-256-gcm", "aes_256_gcm":
		return shadowsocks.CipherType_AES_256_GCM
	case "chacha20-poly1305", "chacha20_poly1305":
		return shadowsocks.CipherType_CHACHA20_POLY1305
	case "xchacha20-poly1305", "xchacha20_poly1305":
		return shadowsocks.CipherType_XCHACHA20_POLY1305
	case "none":
		return shadowsocks.CipherType_NONE
	default:
		return shadowsocks.CipherType_UNKNOWN
	}
}

func ensureBase64Password(password, method string) string {
	decoded, err := base64.StdEncoding.DecodeString(password)
	if err == nil {
		if (strings.Contains(method, "aes-128-gcm") && len(decoded) == 16) ||
			((strings.Contains(method, "aes-256-gcm") || strings.Contains(method, "chacha20-poly1305")) && len(decoded) == 32) {
			return password
		}
	}

	sum := sha256.Sum256([]byte(password))
	keyBytes := sum[:32]
	if strings.Contains(method, "aes-128-gcm") {
		keyBytes = sum[:16]
	}
	return base64.StdEncoding.EncodeToString(keyBytes)
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
