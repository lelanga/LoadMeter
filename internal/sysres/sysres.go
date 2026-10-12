// Package sysres samples the load generator's own CPU and memory usage.
package sysres

import (
	"runtime"
	"time"
)

// Sampler computes process CPU% between calls.
type Sampler struct {
	lastCPU  time.Duration
	lastWall time.Time
}

func NewSampler() *Sampler {
	return &Sampler{lastCPU: processCPU(), lastWall: time.Now()}
}

// Sample returns CPU percent (of one core, may exceed 100 on multi-core) and resident memory in MB.
func (s *Sampler) Sample() (cpuPercent, memMB float64) {
	now := time.Now()
	cpu := processCPU()
	wall := now.Sub(s.lastWall)
	if wall > 0 && cpu > 0 {
		cpuPercent = float64(cpu-s.lastCPU) / float64(wall) * 100
	}
	s.lastCPU, s.lastWall = cpu, now
	memMB = residentMB()
	if memMB == 0 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		memMB = float64(m.Sys) / (1 << 20)
	}
	return cpuPercent, memMB
}
