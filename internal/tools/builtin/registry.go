package builtin

import "noskill/internal/tools"

// Registry returns every builtin tool the built-in skills use.
func Registry() tools.Registry {
	r := tools.Registry{}
	r.Add(Web()...)
	r.Add(Files()...)
	r.Add(Models()...)
	r.Add(Code()...)
	r.Add(Anime()...)
	r.Add(Watcher()...)
	r.Add(Docs()...)
	return r
}
