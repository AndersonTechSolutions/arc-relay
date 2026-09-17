package store

import (
	"encoding/json"
	"fmt"
)

// CoveredUUIDs returns the message UUIDs that a *successful* extraction row
// already sent to mem0 for sessionID. Error rows are excluded in SQL rather
// than after decoding, so a session with a long failure history does not
// pay to re-read it on every pass.
func (s *ExtractionStore) CoveredUUIDs(sessionID string) (map[string]bool, error) {
	rows, err := s.db.Query(`
SELECT chunk_msg_uuids FROM memory_extractions
WHERE session_id = ? AND error IS NULL`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("covered uuids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var uuids []string
		if json.Unmarshal([]byte(blob), &uuids) != nil {
			continue
		}
		for _, u := range uuids {
			out[u] = true
		}
	}
	return out, rows.Err()
}

// PruneErrors deletes failed extraction rows older than `before` (unix
// seconds). Success rows are provenance and are never pruned.
func (s *ExtractionStore) PruneErrors(before float64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM memory_extractions WHERE error IS NOT NULL AND extracted_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("prune extraction errors: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
