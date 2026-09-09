package queue

import "context"

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{ByState: make(map[State]int)}

	rows, err := s.db.QueryContext(ctx, "select state, count(*) from items group by state")
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state State
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return st, err
		}
		st.ByState[state] = n
		st.Total += n
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	if err := s.db.QueryRowContext(ctx,
		"select coalesce(sum(local_bytes),0) from items where local_path is not null").
		Scan(&st.LocalBytes); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx,
		"select count(*), coalesce(sum(state = 'done'),0) from item_targets").
		Scan(&st.TargetsTotal, &st.TargetsDone); err != nil {
		return st, err
	}
	return st, nil
}

// InFlightBytes is the total weight of files sitting on disk waiting to be
// uploaded, which is what the pipeline budget tracks.
func (s *Store) InFlightBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"select coalesce(sum(local_bytes),0) from items "+
			"where local_path is not null and state in ('downloaded','uploading')").Scan(&n)
	return n, err
}

func (s *Store) FailedItems(ctx context.Context, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		"select "+itemColumns+" from items where state = 'failed' order by updated_at desc limit ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// ItemsOnDisk lists videos whose files are still in the work directory. The
// disk budget has to account for whatever a previous run left behind.
func (s *Store) ItemsOnDisk(ctx context.Context) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx,
		"select "+itemColumns+" from items "+
			"where local_path is not null and state in ('downloaded','uploading')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// HasWork reports whether the pipeline still has anything to do.
func (s *Store) HasWork(ctx context.Context, maxAttempts int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"select (select count(*) from items where state = 'downloading' "+
			"   or (state = 'pending' and attempts < ?)) + "+
			"(select count(*) from item_targets t join items i on i.id = t.item_id "+
			"   where t.state = 'uploading' "+
			"      or (t.state = 'pending' and t.attempts < ? and i.state in ('downloaded','uploading')))",
		maxAttempts, maxAttempts).Scan(&n)
	return n > 0, err
}
