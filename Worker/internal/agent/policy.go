package agent

import (
	"fmt"
	"strings"
)

// CheckHost validates a host command against the policy. Returned error is
// user-facing (safe to echo to the agent).
func (p *CommandPolicy) CheckHost(command string) error {
	if !p.AllowHostExec {
		return fmt.Errorf("host command execution is disabled by policy (commandPolicy.allowHostExec=false)")
	}
	return p.check(command)
}

// CheckContainer validates a container exec command against the policy.
func (p *CommandPolicy) CheckContainer(command string) error {
	if !p.AllowContainerExec {
		return fmt.Errorf("container exec is disabled by policy (commandPolicy.allowContainerExec=false)")
	}
	return p.check(command)
}

func (p *CommandPolicy) check(command string) error {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return fmt.Errorf("empty command")
	}
	// Block shell metacharacter chains that allow arbitrary command injection
	// through a whitelisted prefix (e.g. "cat /etc/passwd; rm -rf /").
	switch p.Mode {
	case "whitelist":
		ok := false
		for _, w := range p.Whitelist {
			if strings.HasPrefix(cmd, w) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("command %q is not in the whitelist", firstToken(cmd))
		}
		// Even whitelisted prefixes must not smuggle extra commands.
		if hasShellChain(cmd) {
			return fmt.Errorf("command contains shell chaining operators; not allowed under whitelist mode")
		}
	default: // blacklist
		for _, b := range p.Blacklist {
			if strings.Contains(cmd, b) {
				return fmt.Errorf("command matches blacklist pattern %q", b)
			}
		}
	}
	return nil
}

// firstToken returns the command name (first whitespace-separated token).
func firstToken(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// hasShellChain reports whether the command contains shell chaining operators
// (`;`, `&&`, `||`, `|`, backticks, `$(`) that could smuggle extra commands.
func hasShellChain(cmd string) bool {
	for _, op := range []string{";", "&&", "||", "|", "`", "$("} {
		if strings.Contains(cmd, op) {
			return true
		}
	}
	return false
}
