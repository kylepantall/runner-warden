package hostsetup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Commander runs a command line on the machine with stdin (an SSH session).
type Commander interface {
	Command(ctx context.Context, cmd, stdin string) (stdout, stderr string, exit int, err error)
}

// Check is one finding about a machine. Fix, when set, is a step the wizard
// can run for the operator (after asking).
type Check struct {
	Name   string
	OK     bool
	Detail string
	Fix    func(ctx context.Context) error
	FixMsg string // what Fix does, for the question
}

func run(ctx context.Context, c Commander, cmd, stdin string) (string, error) {
	out, errs, code, err := c.Command(ctx, cmd, stdin)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errs+" "+out))
	}
	return out, nil
}

// sh runs a POSIX script as root on a Linux machine through exec.
func sh(ctx context.Context, c Commander, exec, script string) (string, error) {
	return run(ctx, c, exec+" sh -s", script)
}

// LinuxChecks inspects a Linux machine (or WSL distro) reached through exec:
// root, systemd, Docker Engine.
func LinuxChecks(ctx context.Context, c Commander, exec string, wslDistro string) []Check {
	out, err := sh(ctx, c, exec, `id -u; systemctl is-system-running 2>/dev/null || true
command -v dockerd >/dev/null && echo dockerd=yes || echo dockerd=no
docker info --format '{{.ServerVersion}} {{.CgroupDriver}}' 2>/dev/null || echo docker=down
uname -m`)
	if err != nil {
		return []Check{{Name: "root through exec", Detail: err.Error()}}
	}
	f := strings.Fields(out)
	has := func(s string) bool { return strings.Contains(out, s) }
	checks := []Check{
		{Name: "root through exec", OK: len(f) > 0 && f[0] == "0", Detail: "commands must run as root (exec " + strconv.Quote(exec) + ")"},
		{Name: "systemd", OK: has("running") || has("degraded"), Detail: "the Docker proxy runs as a systemd unit"},
	}
	docker := Check{Name: "Docker Engine", OK: has("dockerd=yes") && !has("docker=down"), Detail: "install Docker Engine (not Docker Desktop)"}
	if !docker.OK {
		docker.FixMsg = "Install Docker Engine (apt install docker.io) and start it"
		docker.Fix = func(ctx context.Context) error {
			_, err := sh(ctx, c, exec, `set -e
export DEBIAN_FRONTEND=noninteractive
command -v apt-get >/dev/null || { echo "only apt-based distros are installed automatically" >&2; exit 1; }
apt-get update -qq && apt-get install -y -qq docker.io >/dev/null
systemctl enable --now docker`)
			return err
		}
	}
	checks = append(checks, docker)
	if wslDistro != "" && !has("running") && !has("degraded") {
		checks[1].Detail = "turn systemd on: [boot] systemd=true in /etc/wsl.conf, then wsl --shutdown"
	}
	return checks
}

// decodeUTF16 decodes wsl.exe's UTF-16LE output (it also passes plain text).
func decodeUTF16(s string) string {
	b := []byte(s)
	if len(b) < 2 || b[1] != 0 {
		return s
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

// WSLDistros lists the WSL distros on a Windows machine, default first.
func WSLDistros(ctx context.Context, c Commander) ([]string, error) {
	out, err := run(ctx, c, "wsl -l -q", "")
	if err != nil {
		return nil, err
	}
	var ds []string
	for _, l := range strings.Split(decodeUTF16(out), "\n") {
		l = strings.TrimSpace(strings.Trim(l, "\x00\r\ufeff"))
		if l != "" && !strings.HasPrefix(l, "docker-desktop") {
			ds = append(ds, l)
		}
	}
	return ds, nil
}

// KeepaliveTask is the Windows scheduled task that keeps a WSL distro up.
func KeepaliveTask(distro string) string { return "runner-warden WSL keepalive (" + distro + ")" }

// WSLKeepaliveCheck reports whether the keepalive task exists, with a Fix
// that registers it: WSL stops a distro with no session attached, and its
// running jobs with it.
func WSLKeepaliveCheck(ctx context.Context, c Commander, distro string) Check {
	name := KeepaliveTask(distro)
	ps := func(script string) (string, error) {
		u := utf16.Encode([]rune("$ProgressPreference='SilentlyContinue'\n" + script))
		b := make([]byte, 0, len(u)*2)
		for _, r := range u {
			b = append(b, byte(r&0xff), byte(r>>8)) //nolint:gosec // masked: low and high byte of a UTF-16 unit
		}
		return run(ctx, c, "powershell -NoProfile -NonInteractive -EncodedCommand "+base64.StdEncoding.EncodeToString(b), "")
	}
	out, _ := ps(`if (Get-ScheduledTask -TaskName '` + name + `' -ErrorAction SilentlyContinue) { 'present' } else { 'missing' }`)
	ch := Check{Name: "WSL keepalive", OK: strings.Contains(out, "present"), Detail: "WSL stops an idle distro, killing running jobs"}
	if !ch.OK {
		ch.FixMsg = "Add a startup task that keeps " + distro + " running (wsl -- sleep infinity)"
		ch.Fix = func(context.Context) error {
			_, err := ps(`$ErrorActionPreference = 'Stop'
$a = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\wsl.exe" -Argument '-d ` + distro + ` -u root -- sleep infinity'
$t = New-ScheduledTaskTrigger -AtStartup
$p = New-ScheduledTaskPrincipal -UserId "$env:USERNAME" -LogonType S4U -RunLevel Highest
$s = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName '` + name + `' -Action $a -Trigger $t -Principal $p -Settings $s -Force | Out-Null
Start-ScheduledTask -TaskName '` + name + `'`)
			return err
		}
	}
	return ch
}

// TartVM is one VM tart knows on a Mac.
type TartVM struct {
	Name    string `json:"Name"`
	Source  string `json:"Source"`
	Running bool   `json:"Running"`
}

// TartVMs lists the Mac's local tart VMs.
func TartVMs(ctx context.Context, c Commander, tart string) ([]TartVM, error) {
	out, err := run(ctx, c, tart+" list --format json", "")
	if err != nil {
		return nil, err
	}
	var all, local []TartVM
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		return nil, err
	}
	for _, v := range all {
		if v.Source == "local" {
			local = append(local, v)
		}
	}
	return local, nil
}

// MacChecks inspects a Mac's VM: running, guest agent, Xcode, and the guest
// user's uid (returned for the config).
func MacChecks(ctx context.Context, c Commander, tart, vm string) ([]Check, int) {
	out, err := run(ctx, c, tart+" exec -i "+vm+" /bin/sh -s", "id -u; xcodebuild -version 2>/dev/null | head -1 || true\n")
	if err != nil {
		return []Check{{Name: "tart guest agent", Detail: "start the VM (tart run " + vm + " --no-graphics, kept up by a launchd agent): " + err.Error()}}, 0
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	uid, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
	xcode := len(lines) > 1 && strings.HasPrefix(lines[1], "Xcode")
	detail := "only needed for Xcode builds"
	if xcode {
		detail = lines[1]
	}
	return []Check{
		{Name: "tart guest agent", OK: true, Detail: "tart exec works"},
		{Name: "Xcode in the guest", OK: xcode, Detail: detail},
	}, uid
}
