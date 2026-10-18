//go:build !darwin

package platform

// AppPref reads one of an app's preferences. Apps on Linux and Windows keep theirs in files the
// caller reads itself.
func AppPref(domain, key string) (string, bool) { return "", false }
