.PHONY: local-db local-db-env test test-integration vet

local-db:
	local-pg guard knowledge-core

local-db-env:
	local-pg env knowledge-core --target host --format shell

test:
	env GOCACHE=/private/tmp/qm-backend-gocache go test ./...

test-integration:
	env GOCACHE=/private/tmp/qm-backend-gocache go test ./...

vet:
	env GOCACHE=/private/tmp/qm-backend-gocache go vet ./...
