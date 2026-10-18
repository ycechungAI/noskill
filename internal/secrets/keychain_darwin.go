package secrets

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

const (
	service   = "noskill"
	b64Prefix = "b64:"
)

// OSKeychain uses the login keychain through /usr/bin/security. The value goes in on standard
// input (security -i), never on the command line where `ps` would show it.
func OSKeychain() Store { return keychain{} }

type keychain struct{}

func (keychain) Get(name string) (string, error) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", service, "-a", name, "-w").Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExit(err, &ee); ok && ee.ExitCode() == 44 { // errSecItemNotFound
			return "", ErrNotFound
		}
		return "", fmt.Errorf("keychain: %v", err)
	}
	v := strings.TrimRight(string(out), "\n")
	// `security -w` prints a hex dump for values with non-ASCII characters, and a hex-looking API
	// key can't be told from one, so values are stored as "b64:<base64>" and decoded here.
	if enc, ok := strings.CutPrefix(v, b64Prefix); ok {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return "", fmt.Errorf("keychain: item %s is damaged", name)
		}
		return string(raw), nil
	}
	return v, nil // an item stored by another tool
}

func (keychain) Set(name, value string) error {
	if strings.ContainsAny(name, "\"\\ \n") {
		return fmt.Errorf("secret name %q can't be used with the macOS keychain", name)
	}
	enc := b64Prefix + base64.StdEncoding.EncodeToString([]byte(value))
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", service, name, enc))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain: %v", err)
	}
	if strings.Contains(stderr.String(), "security:") && strings.Contains(strings.ToLower(stderr.String()), "error") {
		return fmt.Errorf("keychain: %s", strings.TrimSpace(strings.ReplaceAll(stderr.String(), value, "[secret]")))
	}
	return nil
}

func (keychain) Delete(name string) error {
	out, err := exec.Command("/usr/bin/security", "delete-generic-password", "-s", service, "-a", name).CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("could not be found")) {
			return ErrNotFound
		}
		return fmt.Errorf("keychain: %v", err)
	}
	return nil
}
