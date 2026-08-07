// Host-level resource sampling for node views. The worker container shares the
// host's kernel procfs — /proc/stat and /proc/meminfo reflect the HOST, not the
// container (PID namespace isolation does not apply to these global files). So
// reading them inside the worker yields the host's CPU/memory usage, which is
// what the node card and monitor tab should show (previously the UI showed the
// swarm-container aggregate instead).
package nodeagent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// hostCPUStat is a snapshot of the host's aggregate CPU counters from /proc/stat.
type hostCPUStat struct {
	total uint64 // all states summed
	idle  uint64 // idle + iowait
}

// readHostCPUStat reads the first "cpu " line of /proc/stat (aggregate across
// all cores — the same file a bare-metal host reads).
func readHostCPUStat() (hostCPUStat, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return hostCPUStat{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop the "cpu" token
		var total uint64
		for _, v := range fields {
			n, _ := strconv.ParseUint(v, 10, 64)
			total += n
		}
		// fields: user nice system idle iowait irq softirq steal ...
		var idle uint64
		if len(fields) >= 4 {
			idle, _ = strconv.ParseUint(fields[3], 10, 64)
		}
		if len(fields) >= 5 {
			iowait, _ := strconv.ParseUint(fields[4], 10, 64)
			idle += iowait
		}
		return hostCPUStat{total: total, idle: idle}, nil
	}
	return hostCPUStat{}, sc.Err()
}

// hostCPUPercent computes the host-wide CPU usage percent between two snapshots
// taken ~1s apart.
func hostCPUPercent(first, second hostCPUStat) float64 {
	dt := second.total - first.total
	if dt == 0 {
		return 0
	}
	busy := dt - (second.idle - first.idle)
	if busy < 0 {
		busy = 0
	}
	return float64(busy) / float64(dt) * 100
}

// hostMem reads the host's memory totals from /proc/meminfo (values in kB).
func hostMem() (total, used uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	// Scan once, collect every key (a Scanner over the same file cannot be
	// restarted from the top for a second pass).
	vals := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		n, _ := strconv.ParseUint(fields[1], 10, 64)
		vals[key] = n * 1024 // kB -> bytes
	}
	// MemTotal / MemAvailable exist on all kernels >= 3.14.
	total = vals["MemTotal"]
	if avail, ok := vals["MemAvailable"]; ok && total > avail {
		used = total - avail
	}
	return total, used
}
