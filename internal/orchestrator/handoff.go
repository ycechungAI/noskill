package orchestrator

import (
	"fmt"

	"noskill/internal/backends"
)

// Handoff decides swap rule 8 (ORCHESTRATOR.md "When to swap the primary model"): a delegation
// loaded next and pushed the driver out of memory, so going on with the driver means swapping
// back. next takes over when it can drive this run (chat, tool calling when tools are on, enough
// context for what's been said so far) and is at most minGain worse on the task (bandit posterior
// means). The reason is "" when it can't.
func Handoff(c Classification, driver, next Profile, evDriver, evNext Evidence, needCtx int, minGain float64) (bool, string) {
	switch {
	case !next.Info.Has(backends.CapChat):
		return false, ""
	case c.Has("tools") && !next.Info.Has(backends.CapTools) && !next.Info.Has(backends.CapSchema):
		return false, ""
	case c.Has("vision") && !next.Info.Has(backends.CapVision):
		return false, ""
	case next.Ctx > 0 && next.Ctx < needCtx:
		return false, ""
	}
	qd := betaMean(posterior(driver.quality(c.TaskType), evDriver))
	qn := betaMean(posterior(next.quality(c.TaskType), evNext))
	if qn < qd-minGain {
		return false, ""
	}
	return true, fmt.Sprintf("%s is loaded now and finishes the task (%s would need loading again; quality %.2f vs %.2f)",
		next.Info.Name, driver.Info.Name, qn, qd)
}

// BatchOrder orders a batch of independent sub-tasks (swap rule 7): those that run on the loaded
// model (stays) first, then the others grouped by the model they route to, in order of first
// appearance. Unrouted sub-tasks ("") form a group of their own.
func BatchOrder(routed []string, stays []bool) []int {
	var order []int
	for i := range routed {
		if stays[i] {
			order = append(order, i)
		}
	}
	done := map[string]bool{}
	for i, m := range routed {
		if stays[i] || done[m] {
			continue
		}
		done[m] = true
		for j := i; j < len(routed); j++ {
			if !stays[j] && routed[j] == m {
				order = append(order, j)
			}
		}
	}
	return order
}
