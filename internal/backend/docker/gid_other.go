//go:build windows

package docker

import "errors"

func socketGID(string) (int, error) { return 0, errors.New("unsupported platform") }
