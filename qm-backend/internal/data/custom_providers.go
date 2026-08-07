package data

import (
	"context"
	"encoding/json"
	"sort"
)

// CustomProviderRepository is the safe admin projection of Node's
// custom_model_providers durable map. It returns provider specs and key
// presence only; apiKeyEnc is never exposed or decrypted here.
type CustomProviderRepository struct{ pg *Postgres }

func NewCustomProviderRepository(pg *Postgres) *CustomProviderRepository {
	return &CustomProviderRepository{pg: pg}
}

func (r *CustomProviderRepository) Statuses(ctx context.Context) ([]map[string]any, bool, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('custom_model_providers') IS NOT NULL").Scan(&exists); err != nil {
		return nil, false, err
	}
	if !exists {
		return []map[string]any{}, false, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json FROM custom_model_providers ORDER BY id")
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, false, err
		}
		item := map[string]any{}
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		providerID, _ := item["id"].(string)
		if providerID == "" {
			providerID = id
			item["id"] = id
		}
		// Node's statuses() copies the spec then derives these public fields.
		// Do not return the encrypted key even to the admin status view.
		key, _ := item["apiKeyEnc"].(string)
		hasKey := key != ""
		delete(item, "apiKeyEnc")
		if _, present := item["disabled"]; !present {
			item["disabled"] = false
		}
		item["hasKey"] = hasKey
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i]["id"].(string) < result[j]["id"].(string)
	})
	return result, true, nil
}
