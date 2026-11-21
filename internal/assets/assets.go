// Package assets embeds files installed by `runner-warden setup`.
package assets

import _ "embed"

// SystemdUnit is packaging/systemd/runner-warden@.service (kept identical by a test).
//
//go:embed runner-warden@.service
var SystemdUnit []byte

//go:embed runner-warden-update.service
var updateService []byte

//go:embed runner-warden-update.timer
var updateTimer []byte

// UpdateTimer is the unit name of the daily self-update timer.
const UpdateTimer = "runner-warden-update.timer"

// Units maps each systemd unit file name under packaging/systemd to its
// content. setup installs them all; update refreshes them from the release.
var Units = map[string][]byte{
	"runner-warden@.service":       SystemdUnit,
	"runner-warden-update.service": updateService,
	UpdateTimer:                    updateTimer,
}
