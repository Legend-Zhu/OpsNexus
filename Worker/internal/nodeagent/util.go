package nodeagent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type simpleErr struct{ msg string }

func (e simpleErr) Error() string { return e.msg }

func errEmpty(what string) error    { return simpleErr{what + " is empty"} }
func errNotFound(what, id string) error {
	return simpleErr{what + " " + id + " not found"}
}

// joinCmd joins command tokens into a policy-checkable string.
func joinCmd(cmd []string) string {
	return strings.Join(cmd, " ")
}

func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func hostname() (string, error) {
	hn, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("hostname: %w", err)
	}
	return hn, nil
}

// cpuDeltaPercent computes the CPU usage percentage between two stats
// snapshots (docker stats algorithm, normalized by core count).
func cpuDeltaPercent(prev, cur docker.Stats) float64 {
	dt := cur.CPUStats.SystemCPUUsage - prev.CPUStats.SystemCPUUsage
	if dt == 0 {
		return 0
	}
	dtCPU := cur.CPUStats.CPUUsage.TotalUsage - prev.CPUStats.CPUUsage.TotalUsage
	pct := float64(dtCPU) / float64(dt) * 100
	cores := float64(cur.CPUStats.OnlineCPUs)
	if cores > 0 {
		pct *= cores
	}
	return pct
}

// memPercentOf computes memory usage percent, excluding page cache.
func memPercentOf(st docker.Stats) float64 {
	if st.MemoryStats.Limit == 0 {
		return 0
	}
	usage := st.MemoryStats.Usage
	if v, ok := st.MemoryStats.Stats["inactive_file"]; ok && usage > v {
		usage -= v
	}
	return float64(usage) / float64(st.MemoryStats.Limit) * 100
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
