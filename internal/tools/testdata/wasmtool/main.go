// A WASI tool for the wasm runtime tests: built with GOOS=wasip1 GOARCH=wasm.
// Arguments as JSON on stdin; the result as JSON on stdout.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	var in struct {
		A, B   int
		File   string
		Mode   string
		SaveTo string `json:"save_to"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		fmt.Fprintln(os.Stderr, "bad input:", err)
		os.Exit(2)
	}
	switch in.Mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "something broke")
		os.Exit(3)
	case "loop":
		for {
		}
	case "probe":
		// What can this module see? Only /in, /out and /tmp.
		_, passwd := os.ReadFile("/etc/passwd")
		_, home := os.ReadDir("/Users")
		_, root := os.ReadDir("/")
		w := os.WriteFile("/in/x", []byte("x"), 0o644)
		fmt.Printf(`{"passwd_denied":%v,"users_denied":%v,"root_listed":%v,"in_readonly":%v}`, passwd != nil, home != nil, root == nil, w != nil)
		return
	}
	res := map[string]any{"sum": in.A + in.B, "env": os.Getenv("NOSKILL_TOOL")}
	if in.File != "" {
		b, err := os.ReadFile(in.File)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		res["bytes"] = len(b)
		os.WriteFile("/out/copy.txt", b, 0o644)
		res["outputs"] = []map[string]string{{"from": "out/copy.txt", "to": in.SaveTo}}
	}
	json.NewEncoder(os.Stdout).Encode(res)
}
