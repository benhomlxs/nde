PROTOC ?= protoc

__default__:
	@echo "Targets: proto-go build test"

proto-go:
	$(PROTOC) --proto_path=. --go_out=. --go_opt=module=marznode --go-grpc_out=. --go-grpc_opt=module=marznode marznode/service/service.proto
	$(PROTOC) --proto_path=. --go_out=. --go_opt=module=marznode --go_opt=Mmarznode/backends/singbox/sb_stats.proto=marznode/internal/gen/sbstatspb --go-grpc_out=. --go-grpc_opt=module=marznode --go-grpc_opt=Mmarznode/backends/singbox/sb_stats.proto=marznode/internal/gen/sbstatspb marznode/backends/singbox/sb_stats.proto

build:
	go build ./...

test:
	go test ./...
