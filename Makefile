.PHONY: generate local-db local-db-env test test-integration vet

local-db:
	local-pg guard knowledge-core

local-db-env:
	local-pg env knowledge-core --target host --format shell

generate:
	env PATH="$(HOME)/go/bin:$$PATH" protoc --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. api/qm/control/v1/control.proto api/qm/runner/v1/runner.proto api/qm/cron/v1/cron.proto

test:
	env GOCACHE=/private/tmp/qm-backend-gocache go test ./...

test-integration:
	env GOCACHE=/private/tmp/qm-backend-gocache go test ./...

vet:
	env GOCACHE=/private/tmp/qm-backend-gocache go vet ./...
