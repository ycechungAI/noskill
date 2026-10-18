package platform

import "os/exec"

// Windows has no process groups to signal; the Job object (S9 Windows) replaces these.
// Until then only the direct child is killed and usage isn't sampled.

func newProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}

func processGroupUsage(cmd *exec.Cmd) (int64, int) { return 0, 0 }
