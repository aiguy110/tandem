package hostmetrics

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiguy110/tandem/internal/store"
)

const DefaultMaxBytes int64 = 100 * 1024 * 1024

type Service struct {
	store                       *store.Store
	path                        string
	maxBytes                    int64
	previousIdle, previousTotal uint64
}

func New(db *store.Store, path string, maxBytes int64) *Service {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Service{store: db, path: path, maxBytes: maxBytes}
}

func (s *Service) Start(ctx context.Context) {
	slog.Info("host metrics collector started", "interval", 5*time.Second, "history_max_bytes", s.maxBytes, "disk_path", s.path)
	s.collect()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				slog.Info("host metrics collector stopped")
				return
			case <-ticker.C:
				s.collect()
			}
		}
	}()
}

func (s *Service) History(since int64, limit int) ([]store.HostMetric, error) {
	return s.store.HostMetrics(since, limit)
}

func (s *Service) collect() {
	m, err := s.sample()
	if err != nil {
		slog.Warn("host metrics sample failed", "error", err)
		return
	}
	pruned, err := s.store.AddHostMetric(m, s.maxBytes)
	if err != nil {
		slog.Error("host metrics persistence failed", "error", err)
		return
	}
	if pruned > 0 {
		slog.Info("host metrics retention cleanup completed", "rows_deleted", pruned, "history_max_bytes", s.maxBytes)
	}
}

func (s *Service) sample() (store.HostMetric, error) {
	idle, total, err := cpuTimes()
	if err != nil {
		return store.HostMetric{}, err
	}
	cpu := 0.0
	if s.previousTotal > 0 && total > s.previousTotal {
		cpu = 100 * float64((total-s.previousTotal)-(idle-s.previousIdle)) / float64(total-s.previousTotal)
	}
	s.previousIdle, s.previousTotal = idle, total
	memUsed, memTotal, err := memory()
	if err != nil {
		return store.HostMetric{}, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.path, &fs); err != nil {
		return store.HostMetric{}, fmt.Errorf("statfs %s: %w", s.path, err)
	}
	diskTotal := int64(fs.Blocks) * int64(fs.Bsize)
	diskFree := int64(fs.Bavail) * int64(fs.Bsize)
	vu, vt := vram()
	return store.HostMetric{Timestamp: time.Now().UnixMilli(), CPUPercent: cpu, MemoryUsed: memUsed, MemoryTotal: memTotal, DiskUsed: diskTotal - diskFree, DiskTotal: diskTotal, VRAMUsed: vu, VRAMTotal: vt}, nil
}

func cpuTimes() (uint64, uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		return 0, 0, fmt.Errorf("read /proc/stat")
	}
	p := strings.Fields(s.Text())
	if len(p) < 5 || p[0] != "cpu" {
		return 0, 0, fmt.Errorf("invalid /proc/stat cpu line")
	}
	var total uint64
	vals := make([]uint64, len(p)-1)
	for i, raw := range p[1:] {
		vals[i], err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += vals[i]
	}
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4]
	}
	return idle, total, nil
}

func memory() (int64, int64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	vals := map[string]int64{}
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.Fields(line)
		if len(p) >= 2 {
			v, _ := strconv.ParseInt(p[1], 10, 64)
			vals[strings.TrimSuffix(p[0], ":")] = v * 1024
		}
	}
	total := vals["MemTotal"]
	available := vals["MemAvailable"]
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}
	return total - available, total, nil
}

func vram() (*int64, *int64) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, nil
	}
	var used, total int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.Split(line, ",")
		if len(p) != 2 {
			continue
		}
		u, e1 := strconv.ParseInt(strings.TrimSpace(p[0]), 10, 64)
		t, e2 := strconv.ParseInt(strings.TrimSpace(p[1]), 10, 64)
		if e1 == nil && e2 == nil {
			used += u * 1024 * 1024
			total += t * 1024 * 1024
		}
	}
	if total == 0 {
		return nil, nil
	}
	return &used, &total
}
