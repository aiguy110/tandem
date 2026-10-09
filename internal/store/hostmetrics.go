package store

import "fmt"

// HostMetric is one durable sample of this daemon host's resource usage.
// Nullable VRAM values distinguish a host without a supported GPU collector.
type HostMetric struct {
	Timestamp   int64   `json:"ts"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemoryUsed  int64   `json:"memoryUsed"`
	MemoryTotal int64   `json:"memoryTotal"`
	DiskUsed    int64   `json:"diskUsed"`
	DiskTotal   int64   `json:"diskTotal"`
	VRAMUsed    *int64  `json:"vramUsed,omitempty"`
	VRAMTotal   *int64  `json:"vramTotal,omitempty"`
}

func (s *Store) AddHostMetric(m HostMetric, maxBytes int64) (int64, error) {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO host_metrics
                (ts,cpuPercent,memoryUsed,memoryTotal,diskUsed,diskTotal,vramUsed,vramTotal)
                VALUES(?,?,?,?,?,?,?,?)`, m.Timestamp, m.CPUPercent, m.MemoryUsed, m.MemoryTotal, m.DiskUsed, m.DiskTotal, m.VRAMUsed, m.VRAMTotal)
	if err != nil {
		return 0, fmt.Errorf("insert host metric: %w", err)
	}
	// SQLite rows and their b-tree entry average comfortably below 128 bytes.
	// A conservative row cap makes the telemetry allocation deterministic and
	// avoids charging unrelated tables or WAL traffic against this budget.
	maxRows := maxBytes / 128
	if maxRows < 1 {
		maxRows = 1
	}
	result, err := s.db.Exec(`DELETE FROM host_metrics WHERE ts IN
                (SELECT ts FROM host_metrics ORDER BY ts DESC LIMIT -1 OFFSET ?)`, maxRows)
	if err != nil {
		return 0, fmt.Errorf("prune host metrics: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) HostMetrics(since int64, limit int) ([]HostMetric, error) {
	if limit <= 0 || limit > 20000 {
		limit = 20000
	}
	rows, err := s.db.Query(`SELECT ts,cpuPercent,memoryUsed,memoryTotal,diskUsed,diskTotal,vramUsed,vramTotal
                FROM host_metrics WHERE ts >= ? ORDER BY ts ASC LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("query host metrics: %w", err)
	}
	defer rows.Close()
	out := make([]HostMetric, 0)
	for rows.Next() {
		var m HostMetric
		if err := rows.Scan(&m.Timestamp, &m.CPUPercent, &m.MemoryUsed, &m.MemoryTotal, &m.DiskUsed, &m.DiskTotal, &m.VRAMUsed, &m.VRAMTotal); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
