package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parseMemory parses a human-readable memory size into bytes. It accepts
// integer and decimal values with optional binary (Ki/Mi/Gi/Ti/Pi) or
// decimal (K/M/G/T/P) suffixes, case-insensitive. The decimal K/M/G forms are
// treated as power-of-two (1024) to match Docker/k8s conventions. Examples:
//
//	"512Mi" -> 536870912
//	"1G"    -> 1073741824
//	"256m"  -> 268435456
//	"1024"  -> 1024
// ParseMemory parses a human-readable memory size into bytes. See the
// package-level example in parseMemory's doc above.
func ParseMemory(s string) (int64, error) {
	return parseMemory(s)
}

// ParseCPU parses a CPU quantity into NanoCPUs (1 CPU = 1e9).
func ParseCPU(s string) (int64, error) {
	return parseCPU(s)
}

func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty memory value")
	}
	// split numeric prefix from suffix
	var num, suf string
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			continue
		}
		num = s[:i]
		suf = strings.ToLower(strings.TrimSpace(s[i:]))
		goto parse
	}
	num = s
parse:
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
	}
	mult := int64(1)
	switch suf {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1024
	case "ki":
		mult = 1024
	case "m", "mb":
		mult = 1024 * 1024
	case "mi":
		mult = 1024 * 1024
	case "g", "gb":
		mult = 1024 * 1024 * 1024
	case "gi":
		mult = 1024 * 1024 * 1024
	case "t", "tb":
		mult = 1024 * 1024 * 1024 * 1024
	case "ti":
		mult = 1024 * 1024 * 1024 * 1024
	case "p", "pb":
		mult = 1024 * 1024 * 1024 * 1024 * 1024
	case "pi":
		mult = 1024 * 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown memory suffix %q in %q", suf, s)
	}
	return int64(f * float64(mult)), nil
}

// parseCPU parses a CPU quantity into Docker NanoCPUs (1 CPU = 1e9). Accepts
// decimal ("1.0", "0.5") or millicore ("500m") forms. Examples:
//
//	"1.0"  -> 1000000000
//	"0.5"  -> 500000000
//	"500m" -> 500000000
func parseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty cpu value")
	}
	if strings.HasSuffix(strings.ToLower(s), "m") {
		// millicores
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu value %q: %w", s, err)
		}
		return int64(n * 1e6), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu value %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("cpu value must be >= 0, got %q", s)
	}
	return int64(f * 1e9), nil
}
