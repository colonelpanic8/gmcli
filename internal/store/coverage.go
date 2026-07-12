package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	CoverageNotAttempted    = "not_attempted"
	CoverageInProgress      = "in_progress"
	CoveragePartial         = "partial"
	CoverageSourceExhausted = "source_exhausted"
	CoverageFailed          = "failed"
	CoverageComplete        = "complete"
)

type CoverageSegment struct {
	StartMS    int64     `json:"start_ms"`
	EndMS      int64     `json:"end_ms"`
	VerifiedAt time.Time `json:"verified_at"`
}

type ConversationCoverage struct {
	ConversationID     string            `json:"conversation_id"`
	Status             string            `json:"status"`
	HistoryStartMS     *int64            `json:"history_start_ms,omitempty"`
	LastAttemptAt      time.Time         `json:"last_attempt_at,omitempty"`
	LastSuccessAt      time.Time         `json:"last_success_at,omitempty"`
	ExhaustedAt        time.Time         `json:"exhausted_at,omitempty"`
	TerminalReason     string            `json:"terminal_reason,omitempty"`
	LastError          string            `json:"last_error,omitempty"`
	LastRequests       int               `json:"last_requests"`
	LastRecordsFetched int               `json:"last_records_fetched"`
	Segments           []CoverageSegment `json:"segments"`
}

type FolderCoverage struct {
	Folder            string    `json:"folder"`
	Status            string    `json:"status"`
	PagesFetched      int       `json:"pages_fetched"`
	ConversationsSeen int       `json:"conversations_seen"`
	LastAttemptAt     time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt     time.Time `json:"last_success_at,omitempty"`
	TerminalReason    string    `json:"terminal_reason,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
}

func (s *Store) StartConversationCoverage(ctx context.Context, conversationID string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversation_coverage (
			conversation_id, status, last_attempt_ts, updated_at
		) VALUES (?, 'in_progress', ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET
			status = 'in_progress', last_attempt_ts = excluded.last_attempt_ts,
			terminal_reason = '', last_error = '', last_requests = 0,
			last_records_fetched = 0, updated_at = excluded.updated_at`,
		conversationID, now, now)
	return err
}

// RecordConversationCoveragePage persists a successfully traversed cursor
// interval immediately. Overlapping or touching intervals are normalized into
// one segment, keeping the oldest verification time as the weakest claim.
func (s *Store) RecordConversationCoveragePage(ctx context.Context, conversationID string, startMS, endMS int64, requests, records int) error {
	if startMS >= endMS {
		return fmt.Errorf("invalid coverage segment [%d,%d)", startMS, endMS)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	rows, err := tx.QueryContext(ctx, `
		SELECT start_ms, end_ms, verified_at
		  FROM conversation_coverage_segments
		 WHERE conversation_id = ? AND start_ms <= ? AND end_ms >= ?`,
		conversationID, endMS, startMS)
	if err != nil {
		return err
	}
	mergedStart, mergedEnd, verifiedAt := startMS, endMS, now
	type oldSegment struct{ start, end int64 }
	var old []oldSegment
	for rows.Next() {
		var start, end, verified int64
		if err := rows.Scan(&start, &end, &verified); err != nil {
			rows.Close()
			return err
		}
		old = append(old, oldSegment{start, end})
		if start < mergedStart {
			mergedStart = start
		}
		if end > mergedEnd {
			mergedEnd = end
		}
		if verified < verifiedAt {
			verifiedAt = verified
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, segment := range old {
		if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_coverage_segments WHERE conversation_id = ? AND start_ms = ? AND end_ms = ?`, conversationID, segment.start, segment.end); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_coverage_segments (conversation_id, start_ms, end_ms, verified_at) VALUES (?, ?, ?, ?)`, conversationID, mergedStart, mergedEnd, verifiedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversation_coverage
		   SET status = 'partial', last_success_ts = ?, last_requests = ?,
		       last_records_fetched = ?, updated_at = ?
		 WHERE conversation_id = ?`, now, requests, records, now, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishConversationCoverage(ctx context.Context, conversationID, status, reason string, historyStartMS *int64, requests, records int, lastError string) error {
	now := time.Now().UnixMilli()
	exhaustedAt := int64(0)
	if status == CoverageSourceExhausted {
		exhaustedAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE conversation_coverage
		   SET status = ?, terminal_reason = ?, last_error = ?,
		       history_start_ms = COALESCE(?, history_start_ms),
		       exhausted_at = CASE WHEN ? > 0 THEN ? ELSE exhausted_at END,
		       last_requests = ?, last_records_fetched = ?, updated_at = ?
		 WHERE conversation_id = ?`,
		status, reason, lastError, historyStartMS, exhaustedAt, exhaustedAt,
		requests, records, now, conversationID)
	return err
}

func (s *Store) StartFolderCoverage(ctx context.Context, folder string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO folder_coverage (folder, status, last_attempt_ts, updated_at)
		VALUES (?, 'in_progress', ?, ?)
		ON CONFLICT(folder) DO UPDATE SET
			status = 'in_progress', pages_fetched = 0, conversations_seen = 0,
			last_attempt_ts = excluded.last_attempt_ts, terminal_reason = '',
			last_error = '', updated_at = excluded.updated_at`, folder, now, now)
	return err
}

func (s *Store) RecordFolderCoveragePage(ctx context.Context, folder string, conversations int) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		UPDATE folder_coverage
		   SET status = 'partial', pages_fetched = pages_fetched + 1,
		       conversations_seen = conversations_seen + ?, last_success_ts = ?, updated_at = ?
		 WHERE folder = ?`, conversations, now, now, folder)
	return err
}

func (s *Store) FinishFolderCoverage(ctx context.Context, folder, status, reason, lastError string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		UPDATE folder_coverage
		   SET status = ?, terminal_reason = ?, last_error = ?, updated_at = ?
		 WHERE folder = ?`, status, reason, lastError, now, folder)
	return err
}

func (s *Store) ListFolderCoverage(ctx context.Context) ([]FolderCoverage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT folder, status, pages_fetched, conversations_seen, last_attempt_ts, last_success_ts, terminal_reason, last_error FROM folder_coverage ORDER BY folder`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderCoverage
	for rows.Next() {
		var value FolderCoverage
		var attempted, success int64
		if err := rows.Scan(&value.Folder, &value.Status, &value.PagesFetched, &value.ConversationsSeen, &attempted, &success, &value.TerminalReason, &value.LastError); err != nil {
			return nil, err
		}
		value.LastAttemptAt = millisTime(attempted)
		value.LastSuccessAt = millisTime(success)
		out = append(out, value)
	}
	return out, rows.Err()
}

func (s *Store) ListConversationCoverage(ctx context.Context) ([]ConversationCoverage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.conversation_id, COALESCE(cc.status, 'not_attempted'), cc.history_start_ms,
		       COALESCE(cc.last_attempt_ts, 0), COALESCE(cc.last_success_ts, 0),
		       COALESCE(cc.exhausted_at, 0), COALESCE(cc.terminal_reason, ''),
		       COALESCE(cc.last_error, ''), COALESCE(cc.last_requests, 0),
		       COALESCE(cc.last_records_fetched, 0)
		  FROM conversations c LEFT JOIN conversation_coverage cc USING (conversation_id)
		 ORDER BY c.conversation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversationCoverage
	for rows.Next() {
		var value ConversationCoverage
		var history sql.NullInt64
		var attempted, success, exhausted int64
		if err := rows.Scan(&value.ConversationID, &value.Status, &history, &attempted, &success, &exhausted, &value.TerminalReason, &value.LastError, &value.LastRequests, &value.LastRecordsFetched); err != nil {
			return nil, err
		}
		if history.Valid {
			v := history.Int64
			value.HistoryStartMS = &v
		}
		value.LastAttemptAt = millisTime(attempted)
		value.LastSuccessAt = millisTime(success)
		value.ExhaustedAt = millisTime(exhausted)
		value.Segments = []CoverageSegment{}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		segments, err := s.listCoverageSegments(ctx, out[i].ConversationID)
		if err != nil {
			return nil, err
		}
		out[i].Segments = segments
	}
	return out, nil
}

func (s *Store) listCoverageSegments(ctx context.Context, conversationID string) ([]CoverageSegment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT start_ms, end_ms, verified_at FROM conversation_coverage_segments WHERE conversation_id = ? ORDER BY start_ms`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CoverageSegment{}
	for rows.Next() {
		var segment CoverageSegment
		var verified int64
		if err := rows.Scan(&segment.StartMS, &segment.EndMS, &verified); err != nil {
			return nil, err
		}
		segment.VerifiedAt = millisTime(verified)
		out = append(out, segment)
	}
	return out, rows.Err()
}

func millisTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}
