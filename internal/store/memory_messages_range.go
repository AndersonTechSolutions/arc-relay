package store

import "fmt"

// GetSessionRange returns up to limit messages of sessionID with id > afterID,
// ordered by id ASC. This is the extractor's bounded pass: loading by id
// range (not by uuid coverage) means rows below the watermark are never
// reconsidered, including uuid-less ones.
func (s *MessageStore) GetSessionRange(sessionID string, afterID int64, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 400
	}
	rows, err := s.db.Query(`
SELECT id, COALESCE(uuid,''), session_id, COALESCE(parent_uuid,''),
       epoch, timestamp, role, content, platform
FROM memory_messages
WHERE session_id = ? AND id > ?
ORDER BY id ASC
LIMIT ?`, sessionID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get session range: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.UUID, &m.SessionID, &m.ParentUUID,
			&m.Epoch, &m.Timestamp, &m.Role, &m.Content, &m.Platform); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}
