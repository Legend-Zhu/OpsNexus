// Per-node process listing: GET /api/v1/local/processes.
// Pure-Go /proc scanning (no shell), with a ~1s double sample for CPU% —
// same style as the container stats. Non-Linux hosts degrade to an empty
// list; the manager proxies this endpoint per node for the management plane.
package nodeagent

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clkTck 内核时钟滴答/秒（Linux 恒为 100，取 /proc/uptime 不可靠则用默认）。
const clkTck = 100

// processInfo 一个宿主机进程。
type processInfo struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	Cmdline    string  `json:"cmdline,omitempty"`
	State      string  `json:"state"`
	MemKB      uint64  `json:"memKb"`
	CPUPercent float64 `json:"cpuPercent"`
}

type processesResp struct {
	Node      string        `json:"node"`
	Total     int           `json:"total"`
	Processes []processInfo `json:"processes"`
}

// processes handles GET /api/v1/local/processes?limit=100&top=cpu|mem&filter=java.
// top=cpu（默认）按两次采样的 CPU% 降序；top=mem 按 RSS 降序；
// filter 为名称/cmdline 子串（大小写不敏感），先于 sort/limit 应用。
func (a *API) processes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	top := q.Get("top")
	if top == "" {
		top = "cpu"
	}
	filter := strings.ToLower(q.Get("filter"))
	host, _ := hostname()
	first, err := snapshotProcesses()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	time.Sleep(time.Second)
	second, err := snapshotProcesses()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// merge: cpu% from delta ticks, mem/name/cmdline from the second sample
	seen := make(map[int]procSnapshot, len(second))
	for _, p := range second {
		seen[p.pid] = p
	}
	out := make([]processInfo, 0, len(first))
	for _, a0 := range first {
		a1, ok := seen[a0.pid]
		if !ok {
			continue
		}
		if filter != "" && !matchProcess(filter, a1.name, a1.cmdline) {
			continue
		}
		dTick := float64((a1.utime + a1.stime) - (a0.utime + a0.stime))
		pct := dTick / clkTck * 100 // 单核百分比（多核进程可 >100）
		out = append(out, processInfo{
			PID:        a1.pid,
			Name:       a1.name,
			Cmdline:    a1.cmdline,
			State:      a1.state,
			MemKB:      a1.rssPages * 4, // 页大小按 4KiB
			CPUPercent: round2(pct),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if top == "mem" {
			return out[i].MemKB > out[j].MemKB
		}
		return out[i].CPUPercent > out[j].CPUPercent
	})
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, processesResp{Node: host, Total: len(out), Processes: out})
}

// procSnapshot 单次 /proc 采样视图。
type procSnapshot struct {
	pid      int
	name     string
	cmdline  string
	state    string
	utime    uint64
	stime    uint64
	rssPages uint64
}

// snapshotProcesses 扫描 /proc 下全部进程。
func snapshotProcesses() ([]procSnapshot, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var out []procSnapshot
	for _, e := range entries {
		pid, ok := pidFromDir(e)
		if !ok {
			continue
		}
		p, err := readProc(pid)
		if err != nil {
			continue // 进程可能已退出
		}
		out = append(out, p)
	}
	return out, nil
}

func pidFromDir(e fs.DirEntry) (int, bool) {
	if !e.IsDir() {
		return 0, false
	}
	pid, err := strconv.Atoi(e.Name())
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func readProc(pid int) (procSnapshot, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return procSnapshot{}, err
	}
	name, fields, err := parseProcStat(string(stat))
	if err != nil {
		return procSnapshot{}, err
	}
	p := procSnapshot{pid: pid, name: name, state: "?"}
	if len(fields) >= 1 {
		p.state = fields[0]
	}
	// /proc/<pid>/stat 括号后字段（0 基）：0=state 11=utime 12=stime 21=rss(pages)
	if len(fields) >= 13 {
		p.utime = mustUint(fields[11])
		p.stime = mustUint(fields[12])
	}
	if len(fields) >= 22 {
		p.rssPages = mustUint(fields[21])
	}
	cmdline, err := os.ReadFile(filepath.Join(base, "cmdline"))
	if err == nil && len(cmdline) > 0 {
		p.cmdline = strings.ReplaceAll(strings.TrimSuffix(string(cmdline), "\x00"), "\x00", " ")
	}
	if p.cmdline == "" && strings.HasPrefix(p.name, "[") {
		// 内核线程：保留 name 即可
	}
	return p, nil
}

// parseProcStat 解析 /proc/<pid>/stat：comm 可能含空格与括号，取最后一个 ')'。
func parseProcStat(s string) (name string, fields []string, err error) {
	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close <= open {
		return "", nil, errors.New("malformed stat")
	}
	name = s[open+1 : close]
	rest := strings.Fields(s[close+1:])
	return name, rest, nil
}

func mustUint(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// matchProcess filter（已转小写）是否命中进程名或 cmdline（大小写不敏感子串）。
func matchProcess(filter, name, cmdline string) bool {
	return strings.Contains(strings.ToLower(name), filter) ||
		strings.Contains(strings.ToLower(cmdline), filter)
}
