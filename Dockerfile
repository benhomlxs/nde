FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY . .

RUN go build -o /out/marznode ./cmd/marznode

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
WORKDIR /app

COPY --from=builder /out/marznode /app/marznode

CMD ["/app/marznode"]
