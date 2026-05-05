package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var sqlIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)

// SelectTableAsJSONArray runs SELECT * FROM <table> LIMIT n and returns rows as a JSON array of objects.
// Table name must match sqlIdent; otherwise returns empty JSON array.
// If the relation does not exist (42P01), returns [] without error so the API stays up before migrations sync.
func (r *Repository) SelectTableAsJSONArray(ctx context.Context, table string, limit int) (json.RawMessage, error) {
	return r.SelectTableAsJSONArrayCursor(ctx, table, limit, 0)
}

// SelectTableAsJSONArrayCursor runs SELECT * FROM <table> LIMIT n OFFSET k and returns rows as a JSON array.
func (r *Repository) SelectTableAsJSONArrayCursor(ctx context.Context, table string, limit, offset int) (json.RawMessage, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	if offset < 0 {
		offset = 0
	}
	table = strings.TrimSpace(table)
	if !sqlIdent.MatchString(table) {
		return json.RawMessage("[]"), nil
	}

	q := fmt.Sprintf(
		`SELECT coalesce(json_agg(row_to_json(t)), '[]'::json) FROM (SELECT * FROM %s LIMIT $1 OFFSET $2) t`,
		table,
	)

	var raw []byte
	err := r.pool.QueryRow(ctx, q, limit, offset).Scan(&raw)
	if err != nil {
		if isUndefinedTableErr(err, table) {
			return json.RawMessage("[]"), nil
		}
		return nil, fmt.Errorf("realtime query %s: %w", table, err)
	}
	if len(raw) == 0 {
		return json.RawMessage("[]"), nil
	}
	return json.RawMessage(raw), nil
}
