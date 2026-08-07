.PHONY: generate test test-integration vet

generate:
	env PATH="$(HOME)/go/bin:$$PATH" protoc --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. api/qm/control/v1/control.proto api/qm/runner/v1/runner.proto api/qm/cron/v1/cron.proto

test:
	env GOCACHE=/private/tmp/qm-backend-gocache go test ./...

test-integration:
	env GOCACHE=/private/tmp/qm-backend-gocache go test ./...

vet:
	env GOCACHE=/private/tmp/qm-backend-gocache go vet ./...
