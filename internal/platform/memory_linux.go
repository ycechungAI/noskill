package platform

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// ReadMemory reads MemTotal and MemAvailable from /proc/meminfo. GPU memory (nvidia-smi,
// rocm-smi) isn't read yet.
func ReadMemory() (Memory, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	defer f.Close()
	m := Memory{MeasuredAt: time.Now()}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || (k != "MemTotal" && k != "MemAvailable") {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return Memory{}, err
		}
		if k == "MemTotal" {
			m.TotalBytes = kb << 10
		} else {
			m.AvailableBytes = kb << 10
		}
	}
	if m.TotalBytes == 0 {
		return Memory{}, errors.New("MemTotal not found in /proc/meminfo")
	}
	return m, nil
}
