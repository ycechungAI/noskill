package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// OnACPower says whether the machine runs on mains power. known is false when the OS doesn't say;
// callers treat that as on AC (a desktop has no battery to report).
func OnACPower() (on, known bool) {
	dirs, _ := filepath.Glob("/sys/class/power_supply/*")
	sawBattery := false
	for _, d := range dirs {
		typ, _ := os.ReadFile(filepath.Join(d, "type"))
		switch strings.TrimSpace(string(typ)) {
		case "Mains", "USB":
			if online, err := os.ReadFile(filepath.Join(d, "online")); err == nil && strings.TrimSpace(string(online)) == "1" {
				return true, true
			}
		case "Battery":
			sawBattery = true
		}
	}
	if sawBattery {
		return false, true
	}
	return true, false
}
