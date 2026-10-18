package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func newProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// processGroupUsage scans /proc for members of the group (field 5 of stat) and sums their RSS.
func processGroupUsage(cmd *exec.Cmd) (int64, int) {
	if cmd.Process == nil {
		return 0, 0
	}
	pgid := cmd.Process.Pid
	page := int64(os.Getpagesize())
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	var total int64
	n := 0
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')') // comm may contain spaces
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:]) // f[0] = state (field 3)
		if len(f) < 22 {
			continue
		}
		if g, _ := strconv.Atoi(f[2]); g != pgid {
			continue
		}
		rss, _ := strconv.ParseInt(f[21], 10, 64) // field 24
		total += rss * page
		n++
	}
	return total, n
}
