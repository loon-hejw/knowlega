-- +goose Up
CREATE TABLE IF NOT EXISTS secret_drops (
    id TEXT PRIMARY KEY,
    json JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS keychain_credentials (
    id TEXT PRIMARY KEY,
    json JSONB NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS keychain_credentials;
DROP TABLE IF EXISTS secret_drops;
