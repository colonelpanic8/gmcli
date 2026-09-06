package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BindSource prevents live writes from mixing phone-local IDs across pairings.
// A populated pre-binding database cannot be assigned a source retrospectively.
func (s *Store) BindSource(ctx context.Context, fingerprint string) error {
	if fingerprint == "" {
		return errors.New("missing pairing identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM source_identity WHERE id = 1`).Scan(&existing)
	switch {
	case err == nil:
		if existing != fingerprint {
			return errors.New("this database belongs to another pairing; use a new --store directory and pair there; the existing archive remains readable")
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var populated bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM conversations) OR EXISTS(SELECT 1 FROM messages) OR
		EXISTS(SELECT 1 FROM contacts) OR EXISTS(SELECT 1 FROM folder_coverage) OR
		EXISTS(SELECT 1 FROM phone_settings) OR EXISTS(SELECT 1 FROM aliases)`).Scan(&populated); err != nil {
		return err
	}
	if populated {
		return errors.New("legacy database has no verified pairing identity; preserve it as an archive and use a new --store directory for live sync")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_identity (id, fingerprint) VALUES (1, ?)`, fingerprint); err != nil {
		return fmt.Errorf("bind pairing identity: %w", err)
	}
	return tx.Commit()
}
