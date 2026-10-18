package platform

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// NetworkMetered says whether the current connection is metered (FR-J5 "only on unmetered network"),
// from NetworkManager's Metered property over D-Bus (0 unknown, 1 yes, 2 no, 3 guessed yes,
// 4 guessed no). Without NetworkManager it reports unknown; callers treat that as unmetered.
func NetworkMetered() (metered, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "dbus-send", "--system", "--print-reply=literal", "--dest=org.freedesktop.NetworkManager",
		"/org/freedesktop/NetworkManager", "org.freedesktop.DBus.Properties.Get",
		"string:org.freedesktop.NetworkManager", "string:Metered").Output()
	if err != nil {
		return false, false
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return false, false
	}
	switch f[len(f)-1] {
	case "1", "3":
		return true, true
	case "2", "4":
		return false, true
	}
	return false, false
}
