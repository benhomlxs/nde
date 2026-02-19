package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	expcredentials "google.golang.org/grpc/experimental/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"marznode/internal/backend"
	"marznode/internal/backend/hysteria2"
	"marznode/internal/backend/singbox"
	"marznode/internal/backend/xray"
	"marznode/internal/config"
	"marznode/internal/gen/servicepb"
	"marznode/internal/logutil"
	"marznode/internal/service"
	"marznode/internal/storage"
	"marznode/internal/tlsutil"
)

const grpcMaxMessageSize = 64 * 1024 * 1024

func main() {
	cfg := config.Load()
	logutil.Configure(cfg.Debug, cfg.LogFormat, cfg.LogLevel)
	logger := slog.With("component", "main")
	logger.Info("Starting marznode service",
		"address", cfg.ServiceAddress,
		"port", cfg.ServicePort,
		"insecure", cfg.Insecure,
	)

	store := storage.NewMemory()
	backends := make(map[string]backend.Backend)

	if cfg.XrayEnabled {
		b := xray.NewBackend(cfg, store)
		if err := b.Start(context.Background(), ""); err != nil {
			fatal(logger, "start xray failed", "error", err)
		}
		backends["xray"] = b
		logger.Info("Backend core started", "backend", "xray", "version", b.Version())
	}
	if cfg.HysteriaEnabled {
		b := hysteria2.NewBackend(cfg, store)
		if err := b.Start(context.Background(), ""); err != nil {
			fatal(logger, "start hysteria2 failed", "error", err)
		}
		backends["hysteria2"] = b
		logger.Info("Backend core started", "backend", "hysteria2", "version", b.Version())
	}
	if cfg.SingBoxEnabled {
		b := singbox.NewBackend(cfg, store)
		if err := b.Start(context.Background(), ""); err != nil {
			fatal(logger, "start sing-box failed", "error", err)
		}
		backends["sing-box"] = b
		logger.Info("Backend core started", "backend", "sing-box", "version", b.Version())
	}

	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(grpcMaxMessageSize),
		grpc.MaxSendMsgSize(grpcMaxMessageSize),
		grpc.ChainUnaryInterceptor(unaryLogInterceptor(cfg.Debug)),
		grpc.ChainStreamInterceptor(streamLogInterceptor(cfg.Debug)),
	}
	if !cfg.Insecure {
		if err := tlsutil.EnsureServerKeypair(cfg.SSLCertFile, cfg.SSLKeyFile); err != nil {
			fatal(logger, "ensure keypair failed", "error", err)
		}
		tlsCfg, err := tlsutil.BuildServerTLSConfig(cfg.SSLCertFile, cfg.SSLKeyFile, cfg.SSLClientCertFile)
		if err != nil {
			fatal(logger, "build tls failed", "error", err)
		}
		// Marzneshin's grpclib client can connect without ALPN configured on the client SSL context.
		// grpc-go enforces ALPN by default; using compatibility credentials keeps interop with existing panels.
		serverOpts = append(serverOpts, grpc.Creds(expcredentials.NewTLSWithALPNDisabled(tlsCfg)))
	}

	grpcServer := grpc.NewServer(serverOpts...)
	svc := service.New(store, backends)
	servicepb.RegisterMarzServiceServer(grpcServer, svc)

	healthSvc := health.NewServer()
	healthSvc.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthSvc.SetServingStatus("marznode.MarzService", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthSvc)

	addr := net.JoinHostPort(cfg.ServiceAddress, intToString(cfg.ServicePort))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		fatal(logger, "listen failed", "address", addr, "error", err)
	}

	go func() {
		logger.Info("Node service running", "address", addr)
		if err := grpcServer.Serve(lis); err != nil {
			fatal(logger, "grpc server failed", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down marznode")
	grpcServer.GracefulStop()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	for name, b := range backends {
		if err := b.Stop(ctx); err != nil {
			logger.Error("stop backend failed", "name", name, "error", err)
		}
	}
}

func intToString(v int) string {
	return strconv.Itoa(v)
}

func unaryLogInterceptor(debug bool) grpc.UnaryServerInterceptor {
	logger := slog.With("component", "grpc.unary")
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if debug {
			logger.Debug("grpc unary call", "method", info.FullMethod)
		}
		resp, err := handler(ctx, req)
		if err != nil {
			if isExpectedGRPCCancel(err) {
				logger.Info("grpc unary canceled", "method", info.FullMethod)
				return resp, err
			}
			logger.Error("grpc unary error", "method", info.FullMethod, "error", err)
		}
		return resp, err
	}
}

func streamLogInterceptor(debug bool) grpc.StreamServerInterceptor {
	logger := slog.With("component", "grpc.stream")
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if debug {
			logger.Debug("grpc stream call", "method", info.FullMethod)
		}
		err := handler(srv, ss)
		if err != nil {
			if isExpectedGRPCCancel(err) {
				logger.Info("grpc stream canceled", "method", info.FullMethod)
				return err
			}
			logger.Error("grpc stream error", "method", info.FullMethod, "error", err)
		}
		return err
	}
}

func fatal(logger *slog.Logger, msg string, args ...any) {
	logger.Error(msg, args...)
	os.Exit(1)
}

func isExpectedGRPCCancel(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return status.Code(err) == codes.Canceled
}
