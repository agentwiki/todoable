package local

import (
	"os"
	"path/filepath"
	"time"
)

func (s *Store) initLogRetention() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS completion_times(submission_id TEXT PRIMARY KEY REFERENCES submissions(id),completed_at INTEGER NOT NULL);
 INSERT OR IGNORE INTO completion_times SELECT id,? FROM submissions WHERE state='completed'`, time.Now().UnixNano())
	return e
}

type completedLogs struct {
	submission  string
	completedAt int64
	paths       []string
	bytes       uint64
}

// CleanupLogs removes only stdout/stderr belonging to terminal submissions.
// Results, snapshots, process evidence and user artifacts stay in place.
func (s *Store) CleanupLogs() error {
	rows, e := s.db.Query(`SELECT s.id,c.completed_at,r.id,st.id FROM completion_times c JOIN submissions s ON s.id=c.submission_id JOIN runs r ON r.submission_id=s.id JOIN steps st ON st.run_id=r.id WHERE s.state='completed' ORDER BY c.completed_at,s.seq,r.run_seq,st.seq`)
	if e != nil {
		return e
	}
	groups := []completedLogs{}
	for rows.Next() {
		var submission, run, step string
		var completedAt int64
		if e = rows.Scan(&submission, &completedAt, &run, &step); e != nil {
			_ = rows.Close()
			return e
		}
		if len(groups) == 0 || groups[len(groups)-1].submission != submission {
			groups = append(groups, completedLogs{submission: submission, completedAt: completedAt})
		}
		g := &groups[len(groups)-1]
		for _, name := range []string{"stdout", "stderr"} {
			g.paths = append(g.paths, filepath.Join(s.dir, "runs", run, "steps", step, name))
		}
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return e
	}
	var total uint64
	for i := range groups {
		for _, path := range groups[i].paths {
			info, e := os.Lstat(path)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			groups[i].bytes += uint64(info.Size())
		}
		total += groups[i].bytes
	}
	retention, _ := time.ParseDuration(s.config.CompletedLogRetention)
	cutoff := time.Now().Add(-retention).UnixNano()
	for _, g := range groups {
		if g.completedAt > cutoff && total <= uint64(s.config.CompletedLogBytes) {
			continue
		}
		for _, path := range g.paths {
			if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
		total -= g.bytes
	}
	return nil
}
