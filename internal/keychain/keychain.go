// Package keychain wraps the macOS `security(1)` CLI for generic password items.
//
// Claude Code stores its OAuth credentials the same way, so cas talks to the
// keychain through the exact same binary. Secrets are passed as a hex blob on
// `security -i` stdin whenever they fit, which keeps them out of the process
// argument list (visible to any local `ps`).
package keychain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrNotFound is returned when the requested item does not exist.
var ErrNotFound = errors.New("keychain item not found")

const securityBin = "/usr/bin/security"

// stdinLimit is a conservative cap on the length of a single `security -i`
// command line. Longer payloads fall back to argv.
const stdinLimit = 3500

// Get returns the password stored for service/account.
func Get(service, account string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(securityBin, "find-generic-password", "-s", service, "-a", account, "-w")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if strings.Contains(msg, "could not be found") || ee.ExitCode() == 44 {
				return "", ErrNotFound
			}
			return "", fmt.Errorf("keychain read %q: %s", service, firstLine(msg))
		}
		return "", fmt.Errorf("keychain read %q: %w", service, err)
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

// Set creates or updates the item for service/account. label is the name shown
// in Keychain Access; it defaults to the service name when empty.
func Set(service, account, label, value string) error {
	payload := hex.EncodeToString([]byte(value))

	line := "add-generic-password -U -a " + quote(account) + " -s " + quote(service)
	if label != "" {
		line += " -l " + quote(label)
	}
	line += " -X " + quote(payload) + "\n"

	if len(line) <= stdinLimit {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(securityBin, "-i")
		cmd.Stdin = strings.NewReader(line)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		combined := stderr.String() + stdout.String()
		if err == nil && !strings.Contains(combined, "error") {
			return nil
		}
		// Fall through to argv on any trouble; the interactive mode is the
		// optimisation, not the contract.
	}

	args := []string{"add-generic-password", "-U", "-a", account, "-s", service}
	if label != "" {
		args = append(args, "-l", label)
	}
	args = append(args, "-X", payload)

	var stderr bytes.Buffer
	cmd := exec.Command(securityBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain write %q: %s", service, firstLine(stderr.String()))
	}
	return nil
}

// Delete removes the item for service/account. A missing item is not an error.
func Delete(service, account string) error {
	var stderr bytes.Buffer
	cmd := exec.Command(securityBin, "delete-generic-password", "-s", service, "-a", account)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "could not be found") {
			return nil
		}
		return fmt.Errorf("keychain delete %q: %s", service, firstLine(msg))
	}
	return nil
}

// Exists reports whether an item for service/account is present.
func Exists(service, account string) bool {
	cmd := exec.Command(securityBin, "find-generic-password", "-s", service, "-a", account)
	return cmd.Run() == nil
}

// ListServices returns the distinct service names in the login keychain that
// contain substr. Used by `cas doctor` to spot alternate CLAUDE_CONFIG_DIR
// credential entries.
func ListServices(substr string) []string {
	out, err := exec.Command(securityBin, "dump-keychain").Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var services []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		const prefix = `"svce"<blob>="`
		if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, `"`) {
			continue
		}
		name := line[len(prefix) : len(line)-1]
		if !strings.Contains(name, substr) || seen[name] {
			continue
		}
		seen[name] = true
		services = append(services, name)
	}
	return services
}

// quote renders s as a double-quoted token for `security -i`'s argument parser.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "unknown error"
	}
	return s
}
