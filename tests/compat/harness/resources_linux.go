package harness

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Read raw effective restrictions, preserving unavailable fields as errors.
func Resources() map[string]string {
	r := map[string]string{"captured_at": time.Now().UTC().Format(time.RFC3339Nano)}
	read := func(key, path string) {
		b, e := os.ReadFile(path)
		if e != nil {
			r[key] = "unavailable: " + e.Error()
		} else {
			r[key] = string(b)
		}
	}
	read("loadavg", "/proc/loadavg")
	read("self_status", "/proc/self/status")
	read("self_limits", "/proc/self/limits")
	read("self_cgroup", "/proc/self/cgroup")
	read("cpuinfo", "/proc/cpuinfo")
	read("meminfo", "/proc/meminfo")
	for _, line := range strings.Split(r["self_cgroup"], "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			base := filepath.Join("/sys/fs/cgroup", parts[2])
			for _, name := range []string{"cpu.max", "cpu.stat", "cpuset.cpus.effective", "cpuset.mems.effective", "pids.max", "pids.current", "memory.max", "memory.high", "memory.current", "memory.events"} {
				read("cgroup."+name, filepath.Join(base, name))
			}
		}
	}
	return r
}
