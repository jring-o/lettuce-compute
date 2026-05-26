package procmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// dockerContainerReader reads metrics from the Docker/Podman stats API.
type dockerContainerReader struct {
	client *http.Client
}

// NewContainerReader returns a ContainerReader that uses the Docker API.
func NewContainerReader() ContainerReader {
	return &dockerContainerReader{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return net.DialTimeout("unix", "/var/run/docker.sock", 2*time.Second)
				},
			},
			Timeout: 5 * time.Second,
		},
	}
}

// dockerStats is the subset of Docker's container stats response we need.
type dockerStats struct {
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"cpu_stats"`
	PrecpuStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
}

func (r *dockerContainerReader) ReadContainer(containerID string) (*ProcessMetrics, error) {
	if containerID == "" {
		return nil, fmt.Errorf("empty container ID")
	}

	url := fmt.Sprintf("http://localhost/containers/%s/stats?stream=false", containerID)
	resp, err := r.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("docker stats: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker stats returned %d", resp.StatusCode)
	}

	var stats dockerStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decoding stats: %w", err)
	}

	metrics := &ProcessMetrics{}

	// Memory
	rss := float64(stats.MemoryStats.Usage) / (1024 * 1024)
	metrics.MemoryRSSMB = &rss

	// CPU %
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PrecpuStats.CPUUsage.TotalUsage)
	sysDelta := float64(stats.CPUStats.SystemCPUUsage - stats.PrecpuStats.SystemCPUUsage)
	if sysDelta > 0 {
		cpuPct := (cpuDelta / sysDelta) * 100.0
		metrics.CPUUsagePct = &cpuPct
	}

	return metrics, nil
}
