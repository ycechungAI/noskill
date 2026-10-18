package platform

// LibraryDirs: Windows DLLs load from the executable's folder and System32, which the runtime's
// own folder already covers.
func LibraryDirs(exe string) []string { return nil }
