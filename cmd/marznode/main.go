package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	expcredentials "google.golang.org/grpc/experimental/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"marznode/internal/backend"
	"marznode/internal/backend/hysteria2"
	"marznode/internal/backend/singbox"
	"marznode/internal/backend/xray"
	"marznode/internal/config"
	"marznode/internal/gen/servicepb"
	"marznode/internal/service"
	"marznode/internal/storage"
	"marznode/internal/tlsutil"
)

const grpcMaxMessageSize = 64 * 1024 * 1024

func main() {
	cfg := config.Load()

	store := storage.NewMemory()
	backends := make(map[string]backend.Backend)

	if cfg.XrayEnabled {
		b := xray.NewBackend(cfg, store)
		if err := b.Start(context.Background(), ""); err != nil {
			log.Fatalf("start xray failed: %v", err)
		}
		backends["xray"] = b
	}
	if cfg.HysteriaEnabled {
		b := hysteria2.NewBackend(cfg, store)
		if err := b.Start(context.Background(), ""); err != nil {
			log.Fatalf("start hysteria2 failed: %v", err)
		}
		backends["hysteria2"] = b
	}
	if cfg.SingBoxEnabled {
		b := singbox.NewBackend(cfg, store)
		if err := b.Start(context.Background(), ""); err != nil {
			log.Fatalf("start sing-box failed: %v", err)
		}
		backends["sing-box"] = b
	}

	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(grpcMaxMessageSize),
		grpc.MaxSendMsgSize(grpcMaxMessageSize),
	}
	if !cfg.Insecure {
		if err := tlsutil.EnsureServerKeypair(cfg.SSLCertFile, cfg.SSLKeyFile); err != nil {
			log.Fatalf("ensure keypair failed: %v", err)
		}
		tlsCfg, err := tlsutil.BuildServerTLSConfig(cfg.SSLCertFile, cfg.SSLKeyFile, cfg.SSLClientCertFile)
		if err != nil {
			log.Fatalf("build tls failed: %v", err)
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
		log.Fatalf("listen failed: %v", err)
	}

	go func() {
		log.Printf("marznode go service listening on %s", addr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("grpc server failed: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down marznode")
	grpcServer.GracefulStop()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	for name, b := range backends {
		if err := b.Stop(ctx); err != nil {
			log.Printf("stop backend %s failed: %v", name, err)
		}
	}
}

func intToString(v int) string {
	return strconv.Itoa(v)
}
