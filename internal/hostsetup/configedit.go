// Package hostsetup links remote machines to an instance: it edits the
// config file in place (keeping comments) and checks and prepares the
// machine over SSH. The CLI wizard (`runner-warden host add`) drives it.
package hostsetup

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kylepantall/runner-warden/internal/core"
)

// Kind is the type of machine being linked.
type Kind string

// Machine kinds.
const (
	KindLinux   Kind = "linux"   // [[linux.host]] over plain SSH
	KindWSL     Kind = "wsl"     // [[linux.host]] in a WSL distro behind Windows OpenSSH
	KindWindows Kind = "windows" // [[windows.host]]
	KindMac     Kind = "macos"   // [[macos.host]] with a tart VM
)

// OS is the job OS a kind serves.
func (k Kind) OS() core.OS {
	switch k {
	case KindWindows:
		return core.Windows
	case KindMac:
		return core.MacOS
	}
	return core.Linux
}

// Host is everything the config needs for one machine.
type Host struct {
	Kind          Kind
	Name          string
	Address       string
	SSHUser       string
	Targets       []string
	MaxConcurrent int    // linux, wsl, windows
	Exec          string // linux, wsl
	VM            string // macos
	GuestUID      int    // macos
}

func quote(s string) string { return strconv.Quote(s) }

func list(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// Block renders the host's TOML table.
func (h Host) Block() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[[%s.host]]\n", h.Kind.OS())
	fmt.Fprintf(&b, "name = %s\naddress = %s\nssh_user = %s\n", quote(h.Name), quote(h.Address), quote(h.SSHUser))
	if h.Exec != "" {
		fmt.Fprintf(&b, "exec = %s\n", quote(h.Exec))
	}
	if h.MaxConcurrent > 0 {
		fmt.Fprintf(&b, "max_concurrent = %d\n", h.MaxConcurrent)
	}
	if h.VM != "" {
		fmt.Fprintf(&b, "vm = %s\n", quote(h.VM))
	}
	if h.GuestUID != 0 && h.GuestUID != 501 {
		fmt.Fprintf(&b, "guest_uid = %d\n", h.GuestUID)
	}
	fmt.Fprintf(&b, "targets = %s\n", list(h.Targets))
	return b.String()
}

var (
	tableRe = regexp.MustCompile(`(?m)^[ \t]*\[{1,2}[ \t]*([A-Za-z0-9_.\-]+)\s*\]{1,2}\s*(#.*)?$`)
	osRe    = regexp.MustCompile(`(?m)^([ \t]*os[ \t]*=[ \t]*)\[([^\]]*)\](.*)$`)
	keyRe   = func(k string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(k) + `[ \t]*=[ \t]*"([^"]*)"`)
	}
	enabledRe = regexp.MustCompile(`(?m)^([ \t]*enabled[ \t]*=[ \t]*)(true|false)(.*)$`)
)

// section is one table of the file: its header line through the next header.
type section struct {
	name       string // e.g. "linux", "target", "macos.host"
	start, end int    // byte offsets; start is the header line
}

func sections(s string) []section {
	idx := tableRe.FindAllStringSubmatchIndex(s, -1)
	out := make([]section, len(idx))
	for i, m := range idx {
		end := len(s)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out[i] = section{name: s[m[2]:m[3]], start: m[0], end: end}
	}
	return out
}

// AddHost returns the config text with h appended, its OS section enabled,
// and its OS added to every [[target]] h serves. Comments and layout stay.
func AddHost(src string, h Host) (string, error) {
	for _, sec := range sections(src) {
		if sec.name == string(h.Kind.OS())+".host" {
			if m := keyRe("name").FindStringSubmatch(src[sec.start:sec.end]); m != nil && m[1] == h.Name {
				return "", fmt.Errorf("a %s host named %q already exists", h.Kind.OS(), h.Name)
			}
		}
	}
	out := enableSection(src, string(h.Kind.OS()))
	out = addOSToTargets(out, h.Kind.OS(), h.Targets)
	return strings.TrimRight(out, "\n") + "\n\n" + h.Block(), nil
}

// enableSection sets enabled = true in [name], adding the table if missing.
func enableSection(src, name string) string {
	for _, sec := range sections(src) {
		if sec.name != name {
			continue
		}
		body := src[sec.start:sec.end]
		if loc := enabledRe.FindStringSubmatchIndex(body); loc != nil {
			body = body[:loc[4]] + "true" + body[loc[5]:]
		} else {
			nl := strings.IndexByte(body, '\n')
			if nl < 0 {
				body += "\n"
				nl = len(body) - 1
			}
			body = body[:nl+1] + "enabled = true\n" + body[nl+1:]
		}
		return src[:sec.start] + body + src[sec.end:]
	}
	return strings.TrimRight(src, "\n") + "\n\n[" + name + "]\nenabled = true\n"
}

// serves mirrors config target matching: "*", the owner, or owner/repo.
func serves(targets []string, org, repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	for _, t := range targets {
		if t == "*" || (org != "" && t == org) || (repo != "" && (t == repo || t == owner)) {
			return true
		}
	}
	return false
}

// addOSToTargets adds os to the os list of each [[target]] the host serves.
// A target without an os line serves Linux only, so it gets both.
func addOSToTargets(src string, os core.OS, targets []string) string {
	secs := sections(src)
	for i := len(secs) - 1; i >= 0; i-- { // back to front: offsets stay valid
		sec := secs[i]
		if sec.name != "target" {
			continue
		}
		body := src[sec.start:sec.end]
		org, repo := "", ""
		if m := keyRe("org").FindStringSubmatch(body); m != nil {
			org = m[1]
		}
		if m := keyRe("repo").FindStringSubmatch(body); m != nil {
			repo = m[1]
		}
		if !serves(targets, org, repo) {
			continue
		}
		if loc := osRe.FindStringSubmatchIndex(body); loc != nil {
			var have []string
			for _, p := range strings.Split(body[loc[4]:loc[5]], ",") {
				if p = strings.Trim(strings.TrimSpace(p), `"'`); p != "" {
					have = append(have, p)
				}
			}
			if slices.Contains(have, string(os)) {
				continue
			}
			have = append(have, string(os))
			body = body[:loc[4]] + strings.TrimPrefix(strings.TrimSuffix(list(have), "]"), "[") + body[loc[5]:]
		} else {
			oses := []string{"linux"}
			if os != core.Linux {
				oses = append(oses, string(os))
			}
			nl := strings.IndexByte(body, '\n')
			if nl < 0 {
				body += "\n"
				nl = len(body) - 1
			}
			body = body[:nl+1] + "os = " + list(oses) + "\n" + body[nl+1:]
		}
		src = src[:sec.start] + body + src[sec.end:]
	}
	return src
}
