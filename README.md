Marznode (Go)
-------------
Go refactor of `marznode` for connecting node servers to the central Marzneshin panel.

Supported backends:
- `xray-core`
- `sing-box`
- `hysteria2`

## Build

```bash
go build -o marznode ./cmd/marznode
```

## Run

```bash
cp .env.example .env
./marznode
```

## Environment

Main variables are in `.env.example`:
- service bind: `SERVICE_ADDRESS`, `SERVICE_PORT`, `INSECURE`
- backend paths: `XRAY_EXECUTABLE_PATH`, `SING_BOX_EXECUTABLE_PATH`, `HYSTERIA_EXECUTABLE_PATH`
- backend config paths: `XRAY_CONFIG_PATH`, `SING_BOX_CONFIG_PATH`, `HYSTERIA_CONFIG_PATH`
- TLS: `SSL_KEY_FILE`, `SSL_CERT_FILE`, `SSL_CLIENT_CERT_FILE`

## Proto Generation

Install:
- `protoc`
- `protoc-gen-go`
- `protoc-gen-go-grpc`

Then:

```bash
make proto-go
```
