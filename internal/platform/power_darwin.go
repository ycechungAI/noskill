package platform

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// OnACPower says whether the machine runs on mains power. known is false when the OS doesn't say;
// callers treat that as on AC (a desktop has no battery to report).
func OnACPower() (on, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "batt").Output()
	if err != nil {
		return true, false
	}
	switch s := string(out); {
	case strings.Contains(s, "'AC Power'"):
		return true, true
	case strings.Contains(s, "'Battery Power'"):
		return false, true
	}
	return true, false
}
