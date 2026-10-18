package platform

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// meteredScript asks WinRT for the internet connection's cost type (Unknown, Unrestricted, Fixed,
// Variable). Untested on a real Windows machine; it uses the documented API only.
const meteredScript = `$p=[Windows.Networking.Connectivity.NetworkInformation,Windows.Networking.Connectivity,ContentType=WindowsRuntime]::GetInternetConnectionProfile(); if ($p) { $p.GetConnectionCost().NetworkCostType }`

// NetworkMetered says whether the current connection is metered (FR-J5 "only on unmetered network"):
// Fixed or Variable cost is metered. Unknown when Windows doesn't say; callers treat that as unmetered.
func NetworkMetered() (metered, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", meteredScript).Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(out)) {
	case "Fixed", "Variable":
		return true, true
	case "Unrestricted":
		return false, true
	}
	return false, false
}
