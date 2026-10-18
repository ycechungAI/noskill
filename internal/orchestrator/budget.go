package orchestrator

import (
	"fmt"

	"noskill/internal/config"
	"noskill/internal/platform"
)

// GB is the unit Ollama reports model sizes in (decimal), and so the unit of the budget.
const GB = 1_000_000_000

// MemoryBudget is how much memory models may use (ORCHESTRATOR.md "Memory budget", FR-O3).
func MemoryBudget(setting config.AutoFloat, m platform.Memory) (int64, string) {
	if !setting.Auto {
		return int64(setting.Value * GB), "memory_budget_gb"
	}
	total, vram := int64(m.TotalBytes), int64(m.VRAMBytes)
	switch {
	case m.AppleSilicon && total > 12<<30 && total <= 18<<30: // 16 GB machines; 8 GB Macs get ⅔
		// Half the machine is the user's other work (ADR-0012); 6 GB of models plus noskill
		// itself stays inside the other half. More when free memory allows (BurstBudget).
		return 6 * GB, "16 GB Apple Silicon: 6 GB, half the machine is yours (ADR-0012)"
	case m.AppleSilicon:
		return total * 2 / 3, "Apple Silicon: ⅔ of RAM (Metal working-set limit)"
	case vram > GB:
		return vram - GB/2, "discrete GPU: VRAM − 0.5 GB"
	case total > 0:
		return total / 2, "CPU only: 50% of RAM"
	}
	return 0, "memory size unknown"
}

// Burst rules (ADR-0012).
const (
	BurstReserve = 2 * GB // free memory left alone for the OS and whatever starts next
)

// BurstCeiling is the most the budget may grow to when memory is free: memory_burst_gb, or with
// "auto" the Metal working-set limit on Apple Silicon (⅔ of RAM) and ¾ of RAM on CPU-only
// machines. A discrete GPU doesn't grow: its VRAM is the limit whatever else runs. 0 = off.
func BurstCeiling(setting config.AutoFloat, base int64, m platform.Memory) int64 {
	if !setting.Auto {
		return max(int64(setting.Value*GB), 0)
	}
	total := int64(m.TotalBytes)
	switch {
	case m.AppleSilicon:
		return total * 2 / 3
	case m.VRAMBytes > GB:
		return base
	case total > 0:
		return total * 3 / 4
	}
	return base
}

// BurstBudget is the budget for one routing decision: the base, grown toward the ceiling by what
// is free right now. ours is what noskill's models already hold (they can be swapped out, so it
// counts as room). Under memory pressure, or with free memory unknown, it is the base.
func BurstBudget(base, ceiling int64, m platform.Memory, ours int64) (int64, string) {
	if ceiling <= base || m.AvailableBytes == 0 || (m.Pressure != "" && m.Pressure != "normal") {
		return base, ""
	}
	room := int64(m.AvailableBytes) + ours - BurstReserve
	if room <= base {
		return base, ""
	}
	b := min(room, ceiling)
	return b, fmt.Sprintf("grown from %s while %s is free (ceiling %s)", gb(base), gb(int64(m.AvailableBytes)), gb(ceiling))
}

func gb(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/GB) }
