package system

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// Metrics represents a snapshot of host system metrics.
type Metrics struct {
	CPUPercent  float64 `json:"cpu_percent"`
	RAMTotal    uint64  `json:"ram_total"`
	RAMUsed     uint64  `json:"ram_used"`
	RAMFree     uint64  `json:"ram_free"`
	RAMPercent  float64 `json:"ram_percent"`
	DiskTotal   uint64  `json:"disk_total"`
	DiskUsed    uint64  `json:"disk_used"`
	DiskFree    uint64  `json:"disk_free"`
	DiskPercent float64 `json:"disk_percent"`
	Uptime      uint64  `json:"uptime"`
	Hostname    string  `json:"hostname"`
	OS          string  `json:"os"`
	Timestamp   int64   `json:"timestamp"`
}

// CollectMetrics gathers current CPU, memory, disk, and host metrics.
// If diskPath is empty, the current working directory or root is used.
func CollectMetrics(ctx context.Context, diskPath string) (*Metrics, error) {
	m := &Metrics{
		OS:        runtime.GOOS,
		Timestamp: time.Now().Unix(),
	}

	// Host info
	if hInfo, err := host.InfoWithContext(ctx); err == nil && hInfo != nil {
		m.Hostname = hInfo.Hostname
		m.Uptime = hInfo.Uptime
	} else {
		if hostName, err := os.Hostname(); err == nil {
			m.Hostname = hostName
		}
	}

	// CPU percent (sample with a minimal wait or zero-duration delta)
	cpuPercents, err := cpu.PercentWithContext(ctx, 100*time.Millisecond, false)
	if err == nil && len(cpuPercents) > 0 {
		m.CPUPercent = cpuPercents[0]
	}

	// Virtual Memory
	if vMem, err := mem.VirtualMemoryWithContext(ctx); err == nil && vMem != nil {
		m.RAMTotal = vMem.Total
		m.RAMUsed = vMem.Used
		m.RAMFree = vMem.Available
		m.RAMPercent = vMem.UsedPercent
	}

	// Disk
	targetPath := diskPath
	if targetPath == "" {
		if runtime.GOOS == "windows" {
			targetPath = "C:\\"
		} else {
			targetPath = "/"
		}
	}

	if dUsage, err := disk.UsageWithContext(ctx, targetPath); err == nil && dUsage != nil {
		m.DiskTotal = dUsage.Total
		m.DiskUsed = dUsage.Used
		m.DiskFree = dUsage.Free
		m.DiskPercent = dUsage.UsedPercent
	}

	return m, nil
}
