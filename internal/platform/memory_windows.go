package platform

import (
	"fmt"
	"time"
	"unsafe"
)

var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// ReadMemory reads total and available RAM. Adapter memory (DXGI) is added in 1b.
func ReadMemory() (Memory, error) {
	var s memoryStatusEx
	s.length = uint32(unsafe.Sizeof(s))
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return Memory{}, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return Memory{TotalBytes: s.totalPhys, AvailableBytes: s.availPhys, MeasuredAt: time.Now()}, nil
}
