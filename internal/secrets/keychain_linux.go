package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// OSKeychain uses the Secret Service (GNOME Keyring, KWallet) through secret-tool. The value is
// passed on standard input. Without secret-tool installed, it returns nil and callers fall back to
// the NOSKILL_SECRET_<NAME> environment variables.
func OSKeychain() Store {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return nil
	}
	return keychain{}
}

type keychain struct{}

func (keychain) Get(name string) (string, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", "noskill", "name", name).Output()
	if err != nil || len(out) == 0 {
		return "", ErrNotFound
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (keychain) Set(name, value string) error {
	cmd := exec.Command("secret-tool", "store", "--label=noskill: "+name, "service", "noskill", "name", name)
	cmd.Stdin = strings.NewReader(value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain: %v: %s", err, strings.TrimSpace(strings.ReplaceAll(stderr.String(), value, "[secret]")))
	}
	return nil
}

func (keychain) Delete(name string) error {
	if err := exec.Command("secret-tool", "clear", "service", "noskill", "name", name).Run(); err != nil {
		return errors.Join(ErrNotFound, err)
	}
	return nil
}
