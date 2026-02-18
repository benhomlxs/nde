FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

ARG GOPROXY=https://goproxy.cn,https://goproxy.io,https://proxy.golang.org,direct
ARG GOSUMDB=off
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o /out/marznode ./cmd/marznode

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
WORKDIR /app

COPY --from=builder /out/marznode /app/marznode

CMD ["/app/marznode"]
