package platform

import (
	"time"

	"golang.org/x/sys/unix"
)

// ReadMemory reads total RAM, whether this is Apple Silicon, and the kernel's memory-pressure level
// (the signal S4 used to set the 8 GB budget on 16 GB machines).
func ReadMemory() (Memory, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Memory{}, err
	}
	m := Memory{TotalBytes: total, MeasuredAt: time.Now()}
	if v, err := unix.SysctlUint32("hw.optional.arm64"); err == nil && v == 1 {
		m.AppleSilicon = true
	}
	if lvl, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		switch lvl {
		case 1:
			m.Pressure = "normal"
		case 2:
			m.Pressure = "warn"
		case 4:
			m.Pressure = "critical"
		}
	}
	// The kernel's own "memory available" percentage, the figure its pressure levels come from.
	if lvl, err := unix.SysctlUint32("kern.memorystatus_level"); err == nil && lvl <= 100 {
		m.AvailableBytes = total / 100 * uint64(lvl)
	}
	return m, nil
}
