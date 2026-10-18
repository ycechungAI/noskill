package platform

import (
	"os/exec"
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

// processGroupUsage sums RSS over the group with ps. ADR-0008 planned proc_pidinfo at ≤ 20 ms;
// that's a libproc call Go can't make without CGO, so v1 samples ps every 50 ms.
func processGroupUsage(cmd *exec.Cmd) (int64, int) {
	if cmd.Process == nil {
		return 0, 0
	}
	out, err := exec.Command("/bin/ps", "-o", "rss=", "-g", strconv.Itoa(cmd.Process.Pid)).Output()
	if err != nil {
		return 0, 0
	}
	var total int64
	n := 0
	for _, f := range strings.Fields(string(out)) {
		if v, err := strconv.ParseInt(f, 10, 64); err == nil {
			total += v << 10
			n++
		}
	}
	return total, n
}
