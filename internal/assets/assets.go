// Package assets embeds files installed by `runner-warden setup`.
package assets

import _ "embed"

// SystemdUnit is packaging/systemd/runner-warden@.service (kept identical by a test).
//
//go:embed runner-warden@.service
var SystemdUnit []byte
