package resource

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

const (
	MiB = int64(1 << 20)
	GiB = int64(1 << 30)
)

type Snapshot struct {
	DiskFree        int64   `json:"disk_free"`
	DiskTotal       int64   `json:"disk_total"`
	MemoryLimit     int64   `json:"memory_limit"`
	MemoryCurrent   int64   `json:"memory_current"`
	MemoryAvailable int64   `json:"memory_available"`
	CPUQuota        float64 `json:"cpu_quota"`
}

type Limits struct {
	MinFree              int64
	ReserveFraction      float64
	MinVolume            int64
	MaxVolume            int64
	RequestedVolume      int64
	MaxUploadConcurrency int
	SliceSize            int64
}

type Policy struct {
	ReserveBytes       int64   `json:"reserve_bytes"`
	VolumeBytes        int64   `json:"volume_bytes"`
	InitialConcurrency int     `json:"initial_concurrency"`
	MaxConcurrency     int     `json:"max_concurrency"`
	CPUQuota           float64 `json:"cpu_quota"`
	MemoryAvailable    int64   `json:"memory_available"`
	DiskFree           int64   `json:"disk_free"`
}

func DefaultLimits() Limits {
	return Limits{
		MinFree:              4 * GiB,
		ReserveFraction:      0.05,
		MinVolume:            64 * MiB,
		MaxVolume:            2 * GiB,
		MaxUploadConcurrency: 16,
		SliceSize:            4 * MiB,
	}
}

func Detect(path string) (Snapshot, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return Snapshot{}, fmt.Errorf("stat filesystem %q: %w", path, err)
	}
	bsize := int64(fs.Bsize)
	s := Snapshot{
		DiskFree:  saturatingMul(int64(fs.Bavail), bsize),
		DiskTotal: saturatingMul(int64(fs.Blocks), bsize),
		CPUQuota:  detectCPUQuota(),
	}
	s.MemoryLimit, s.MemoryCurrent, s.MemoryAvailable = detectMemory()
	return s, nil
}

func Plan(s Snapshot, l Limits) (Policy, error) {
	if l.MinVolume <= 0 || l.MaxVolume < l.MinVolume {
		return Policy{}, errors.New("invalid volume-size bounds")
	}
	if l.SliceSize <= 0 {
		return Policy{}, errors.New("slice size must be positive")
	}
	if l.MaxUploadConcurrency < 1 {
		return Policy{}, errors.New("max upload concurrency must be at least 1")
	}

	reserve := l.MinFree
	fractional := int64(float64(s.DiskTotal) * l.ReserveFraction)
	if fractional > reserve {
		reserve = fractional
	}
	usable := s.DiskFree - reserve
	if usable < l.MinVolume*2 {
		return Policy{}, fmt.Errorf("not enough free space: need reserve plus two %d-byte volumes", l.MinVolume)
	}

	safeVolume := usable / 2 // one sealed volume plus one volume being written
	volume := safeVolume
	if l.RequestedVolume > 0 {
		if l.RequestedVolume > safeVolume {
			return Policy{}, fmt.Errorf("requested volume %d exceeds current safe disk cap %d", l.RequestedVolume, safeVolume)
		}
		volume = l.RequestedVolume
	}
	if volume > l.MaxVolume {
		volume = l.MaxVolume
	}
	if volume < l.MinVolume {
		volume = l.MinVolume
	}
	volume -= volume % 512

	memAvailable := s.MemoryAvailable
	if memAvailable <= 0 && s.MemoryLimit > s.MemoryCurrent {
		memAvailable = s.MemoryLimit - s.MemoryCurrent
	}
	if memAvailable <= 0 {
		memAvailable = 512 * MiB
	}
	// The official SDK buffers a multipart slice once. Budget three slice-sized
	// buffers plus 8 MiB of runtime/network overhead per active worker.
	perWorker := 3*l.SliceSize + 8*MiB
	memCap := int((memAvailable / 3) / perWorker) // never devote over 1/3 RAM to upload buffers
	if memCap < 1 {
		memCap = 1
	}

	cpu := s.CPUQuota
	if cpu <= 0 {
		cpu = float64(runtime.NumCPU())
	}
	cpuCap := int(math.Ceil(cpu * 16)) // upload is network-bound; 0.5 CPU can still drive eight requests
	if cpuCap < 1 {
		cpuCap = 1
	}
	maxConcurrency := minInt(l.MaxUploadConcurrency, memCap, cpuCap)
	initial := maxConcurrency
	if initial > 4 {
		initial = 4
	}

	return Policy{
		ReserveBytes:       reserve,
		VolumeBytes:        volume,
		InitialConcurrency: initial,
		MaxConcurrency:     maxConcurrency,
		CPUQuota:           cpu,
		MemoryAvailable:    memAvailable,
		DiskFree:           s.DiskFree,
	}, nil
}

func detectCPUQuota() float64 {
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 2 && fields[0] != "max" {
			quota, qerr := strconv.ParseFloat(fields[0], 64)
			period, perr := strconv.ParseFloat(fields[1], 64)
			if qerr == nil && perr == nil && quota > 0 && period > 0 {
				return quota / period
			}
		}
	}
	quota, qerr := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period, perr := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if qerr == nil && perr == nil && quota > 0 && period > 0 {
		return float64(quota) / float64(period)
	}
	return float64(runtime.NumCPU())
}

func detectMemory() (limit, current, available int64) {
	limit, lerr := readIntFile("/sys/fs/cgroup/memory.max")
	current, cerr := readIntFile("/sys/fs/cgroup/memory.current")
	if lerr == nil && cerr == nil && limit > 0 && limit < math.MaxInt64/2 {
		reclaimable, _ := readMemoryStat("/sys/fs/cgroup/memory.stat", "inactive_file")
		return limit, current, cgroupMemoryAvailable(limit, current, reclaimable)
	}

	limit, lerr = readIntFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	current, cerr = readIntFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
	if lerr == nil && cerr == nil && limit > 0 && limit < math.MaxInt64/2 {
		reclaimable, err := readMemoryStat("/sys/fs/cgroup/memory/memory.stat", "total_inactive_file")
		if err != nil {
			reclaimable, _ = readMemoryStat("/sys/fs/cgroup/memory/memory.stat", "inactive_file")
		}
		return limit, current, cgroupMemoryAvailable(limit, current, reclaimable)
	}

	meminfo, err := os.Open("/proc/meminfo")
	if err == nil {
		defer meminfo.Close()
		values := map[string]int64{}
		scanner := bufio.NewScanner(meminfo)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 2 {
				v, parseErr := strconv.ParseInt(fields[1], 10, 64)
				if parseErr == nil {
					values[strings.TrimSuffix(fields[0], ":")] = v * 1024
				}
			}
		}
		if values["MemTotal"] > 0 {
			return values["MemTotal"], values["MemTotal"] - values["MemAvailable"], values["MemAvailable"]
		}
	}

	// Portable conservative fallback. Disk backpressure remains authoritative.
	return 2 * GiB, 0, 2 * GiB
}

func readMemoryStat(path, key string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != key {
			continue
		}
		value, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("parse %s from %s: %w", key, path, parseErr)
		}
		if value < 0 {
			return 0, fmt.Errorf("parse %s from %s: negative value %d", key, path, value)
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s missing from %s", key, path)
}

// cgroup memory.current includes reclaimable page cache. Treating it all as
// pinned memory can collapse an I/O-bound uploader to one worker after hashing
// a large file. Count inactive file pages as available, capped by the cgroup
// limit and by the amount currently charged to the cgroup.
func cgroupMemoryAvailable(limit, current, inactiveFile int64) int64 {
	if limit <= 0 {
		return 0
	}
	if current < 0 {
		current = 0
	}
	if inactiveFile < 0 {
		inactiveFile = 0
	}
	if inactiveFile > current {
		inactiveFile = current
	}
	headroom := max64(0, limit-current)
	if inactiveFile >= limit-headroom {
		return limit
	}
	return headroom + inactiveFile
}

func readIntFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, errors.New("unlimited")
	}
	return strconv.ParseInt(s, 10, 64)
}

func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

func minInt(v int, rest ...int) int {
	for _, n := range rest {
		if n < v {
			v = n
		}
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
