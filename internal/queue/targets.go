package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnsureTargets creates upload jobs for every downloaded video that lacks
// them. Called on startup and after each download, so adding a store to the
// config picks up what is already downloaded without refetching it.
func (s *Store) EnsureTargets(ctx context.Context, stores []string) (int, error) {
	if len(stores) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		"insert into item_targets(item_id, store, state, updated_at) "+
			"select id, ?, 'pending', ? from items "+
			"where state in ('downloaded','uploading') "+
			"on conflict(item_id, store) do nothing")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := s.unix()
	created := 0
	for _, name := range stores {
		res, err := stmt.ExecContext(ctx, name, now)
		if err != nil {
			return created, fmt.Errorf("cannot create upload jobs for %s: %w", name, err)
		}
		n, _ := res.RowsAffected()
		created += int(n)
	}
	return created, tx.Commit()
}

func (s *Store) EnsureTargetsFor(ctx context.Context, itemID int64, stores []string) error {
	if len(stores) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := s.unix()
	for _, name := range stores {
		if _, err := tx.ExecContext(ctx,
			"insert into item_targets(item_id, store, state, updated_at) values(?,?,'pending',?) "+
				"on conflict(item_id, store) do nothing", itemID, name, now); err != nil {
			return fmt.Errorf("cannot create upload job for item %d in %s: %w", itemID, name, err)
		}
	}
	return tx.Commit()
}

// ClaimUpload takes one upload job under a lease. Only jobs whose video is
// already downloaded are handed out, which rules out a race between the two
// worker pools.
func (s *Store) ClaimUpload(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (*Target, error) {
	row := s.db.QueryRowContext(ctx,
		"update item_targets set state = 'uploading', lease_owner = ?, lease_until = ?, "+
			"attempts = attempts + 1, updated_at = ? "+
			"where id = (select t.id from item_targets t join items i on i.id = t.item_id "+
			"  where t.state = 'pending' and t.attempts < ? "+
			"    and i.state in ('downloaded','uploading') and i.local_path is not null "+
			"  order by t.id limit 1) "+
			"returning id, item_id, store, state, attempts, coalesce(last_error,''), "+
			"coalesce(remote_key,''), bytes",
		owner, s.now().Add(lease).Unix(), s.unix(), maxAttempts)

	var t Target
	err := row.Scan(&t.ID, &t.ItemID, &t.Store, &t.State, &t.Attempts, &t.LastError,
		&t.RemoteKey, &t.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoWork
	}
	if err != nil {
		return nil, fmt.Errorf("cannot claim an upload job: %w", err)
	}

	item, err := s.Item(ctx, t.ItemID)
	if err != nil {
		return nil, fmt.Errorf("cannot read item %d for upload: %w", t.ItemID, err)
	}
	t.Item = *item
	t.VideoID = item.VideoID
	t.LocalPath = item.LocalPath

	if _, err := s.db.ExecContext(ctx,
		"update items set state = 'uploading', updated_at = ? where id = ? and state = 'downloaded'",
		s.unix(), t.ItemID); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) RenewTargetLease(ctx context.Context, targetID int64, owner string, lease time.Duration) error {
	res, err := s.db.ExecContext(ctx,
		"update item_targets set lease_until = ?, updated_at = ? where id = ? and lease_owner = ?",
		s.now().Add(lease).Unix(), s.unix(), targetID, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("lost the lease on upload %d: another worker took it", targetID)
	}
	return nil
}

func (s *Store) ReleaseUpload(ctx context.Context, targetID int64) error {
	_, err := s.db.ExecContext(ctx,
		"update item_targets set state = 'pending', lease_owner = null, lease_until = null, "+
			"attempts = max(attempts - 1, 0), updated_at = ? where id = ? and state = 'uploading'",
		s.unix(), targetID)
	return err
}

// MarkTargetDone closes one target and reports whether the whole video is
// done, which is when the local file is no longer needed.
func (s *Store) MarkTargetDone(ctx context.Context, targetID int64, remoteKey string, bytes int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := s.unix()
	var itemID int64
	if err := tx.QueryRowContext(ctx,
		"update item_targets set state = 'done', remote_key = ?, bytes = ?, last_error = null, "+
			"lease_owner = null, lease_until = null, updated_at = ? where id = ? returning item_id",
		remoteKey, bytes, now, targetID).Scan(&itemID); err != nil {
		return false, fmt.Errorf("cannot close upload %d: %w", targetID, err)
	}

	var pending int
	if err := tx.QueryRowContext(ctx,
		"select count(*) from item_targets where item_id = ? and state != 'done'",
		itemID).Scan(&pending); err != nil {
		return false, err
	}
	if pending > 0 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"update items set state = 'done', last_error = null, updated_at = ? where id = ?",
		now, itemID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) MarkTargetFailed(ctx context.Context, targetID int64, cause error, permanent bool, maxAttempts int) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := s.db.ExecContext(ctx,
		"update item_targets set state = case when ? or attempts >= ? then 'failed' else 'pending' end, "+
			"last_error = ?, lease_owner = null, lease_until = null, updated_at = ? where id = ?",
		permanent, maxAttempts, msg, s.unix(), targetID)
	return err
}

// MarkTargetlessDone closes a video that has no targets at all. That is the
// plain downloader mode: no stores configured, the file stays on disk, but
// the video still has to leave the queue.
func (s *Store) MarkTargetlessDone(ctx context.Context, itemID int64) error {
	_, err := s.db.ExecContext(ctx,
		"update items set state = 'done', updated_at = ? "+
			"where id = ? and not exists (select 1 from item_targets where item_id = ?)",
		s.unix(), itemID, itemID)
	return err
}

// PruneTargets drops jobs for stores that are no longer in the config.
// Without this a removed store would hold its videos in 'uploading' forever
// and the local files would never be deleted.
func (s *Store) PruneTargets(ctx context.Context, keep []string) (int, error) {
	if len(keep) == 0 {
		res, err := s.db.ExecContext(ctx, "delete from item_targets")
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, 0, len(keep))
	for _, k := range keep {
		args = append(args, k)
	}
	res, err := s.db.ExecContext(ctx,
		"delete from item_targets where store not in ("+placeholders+")", args...)
	if err != nil {
		return 0, fmt.Errorf("cannot prune jobs of vanished stores: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) Targets(ctx context.Context, itemID int64) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx,
		"select id, item_id, store, state, attempts, coalesce(last_error,''), "+
			"coalesce(remote_key,''), bytes from item_targets where item_id = ? order by store", itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.ID, &t.ItemID, &t.Store, &t.State, &t.Attempts,
			&t.LastError, &t.RemoteKey, &t.Bytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
