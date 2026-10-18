package platform

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// AppPref reads one of an app's preferences (macOS user defaults, e.g. org.m0k.transmission RPCPort).
// Only reads; ok is false when it isn't set.
func AppPref(domain, key string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", domain, key).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
