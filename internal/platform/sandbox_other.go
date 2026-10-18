//go:build !darwin

package platform

import "context"

// NewSandbox returns this OS's script sandbox. Linux (Landlock, bubblewrap) and Windows
// (AppContainer + Job object) are built after their S9 runs (PHASE0_GO_NO_GO.md carry-overs);
// until then scripts refuse to run unless [sandbox] mode = "warn".
func NewSandbox() Sandbox { return missingSandbox{} }

type missingSandbox struct{}

func (missingSandbox) Strength() SandboxStrength {
	return SandboxStrength{Mechanism: "none", Files: None, Network: None, Limits: Advisory,
		Note: "S9 hasn't run on this OS yet; scripts refuse to run unless [sandbox] mode = \"warn\""}
}

func (missingSandbox) Run(context.Context, SandboxSpec) (SandboxResult, error) {
	return SandboxResult{}, ErrNoSandbox
}

// MaybeRunSandboxChild is a no-op where there's no re-exec step.
func MaybeRunSandboxChild() {}
