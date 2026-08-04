package agent

import (
	"strings"
	"testing"
)

func TestDefaultBlacklist(t *testing.T) {
	p := DefaultConfig().CommandPolicy
	p.AllowHostExec = true // test the blacklist logic itself; the switch is covered by TestAllowSwitches
	blocked := []string{
		"rm -rf /",
		"rm -rf /var/lib/docker",
		"shutdown -h now",
		"mkfs.ext4 /dev/sdb1",
		"dd if=/dev/zero of=/dev/sda bs=1M",
		"cat /etc/passwd; rm -rf /",
	}
	for _, c := range blocked {
		if err := p.CheckHost(c); err == nil {
			t.Errorf("blacklist mode should reject %q", c)
		}
	}
	allowed := []string{"cat /etc/nginx/nginx.conf", "ls -la /etc", "ps aux"}
	for _, c := range allowed {
		if err := p.CheckHost(c); err != nil {
			t.Errorf("blacklist mode should allow %q, got: %v", c, err)
		}
	}
}

func TestWhitelistMode(t *testing.T) {
	p := DefaultConfig().CommandPolicy
	p.Mode = "whitelist"
	p.AllowHostExec = true
	p.Whitelist = []string{"cat", "ls", "ps", "df", "free"}

	for _, c := range []string{"cat /etc/hostname", "ls /var/log", "ps aux"} {
		if err := p.CheckHost(c); err != nil {
			t.Errorf("whitelist should allow %q: %v", c, err)
		}
	}
	for _, c := range []string{"rm -rf /", "useradd foo", "chmod 777 /etc"} {
		if err := p.CheckHost(c); err == nil {
			t.Errorf("whitelist should reject %q", c)
		}
	}
	// chaining operators rejected even under whitelist
	if err := p.CheckHost("cat /etc/passwd && rm -rf /"); err == nil {
		t.Error("whitelist should reject shell chaining")
	}
}

func TestAllowSwitches(t *testing.T) {
	p := DefaultConfig().CommandPolicy
	p.AllowHostExec = false
	if err := p.CheckHost("ls"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("host exec disabled should error, got %v", err)
	}
	p.AllowContainerExec = false
	if err := p.CheckContainer("ls"); err == nil {
		t.Error("container exec disabled should error")
	}
}

func TestEmptyCommand(t *testing.T) {
	p := DefaultConfig().CommandPolicy
	if err := p.CheckHost("   "); err == nil {
		t.Error("empty command should error")
	}
}

func TestHostExecOffline(t *testing.T) {
	// In environments without nsenter this must return an error, not hang.
	out, code, _, err := RunHostCommand(t.Context(), "echo hi", 2e9)
	if err == nil && code != 0 {
		// nsenter present but command failed — acceptable in CI without host
		t.Logf("host exec ran: out=%q code=%d", out, code)
	}
}
