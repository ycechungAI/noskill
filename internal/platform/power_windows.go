package platform

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetSystemPowerStatus = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemPowerStatus")

// systemPowerStatus is SYSTEM_POWER_STATUS.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// OnACPower says whether the machine runs on mains power. known is false when the OS doesn't say;
// callers treat that as on AC (a desktop has no battery to report).
func OnACPower() (on, known bool) {
	var s systemPowerStatus
	if r, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return true, false
	}
	switch s.ACLineStatus {
	case 0:
		return false, true
	case 1:
		return true, true
	}
	return true, false
}
