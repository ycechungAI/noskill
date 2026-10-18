package platform

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// OpenURL opens an http(s) address in the default browser. The tray uses it for the status page.
func OpenURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("not an http address: %q", raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", raw).Run()
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", raw).Run()
	}
	return exec.CommandContext(ctx, "xdg-open", raw).Run()
}
