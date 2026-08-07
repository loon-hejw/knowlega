FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/qm-backend ./cmd/qm-backend

FROM alpine:3.21

RUN adduser -D -H -u 10001 qm
USER qm
WORKDIR /app
COPY --from=build /out/qm-backend /usr/local/bin/qm-backend
COPY configs/config.yaml /app/config.yaml
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/qm-backend", "--config", "/app/config.yaml"]
