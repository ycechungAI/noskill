package platform

// NetworkMetered says whether the current connection is metered (FR-J5 "only on unmetered network").
// macOS only tells apps through Network.framework (NWPath.isExpensive), which needs CGO, so this
// reports unknown; callers treat unknown as unmetered.
func NetworkMetered() (metered, known bool) { return false, false }
