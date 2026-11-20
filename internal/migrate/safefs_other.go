//go:build !unix

package migrate

import "errors"

// Retiring legacy runners is Linux-only; every no-follow operation is refused
// elsewhere.

var errUnsafePath = errors.New("unsafe path")

var errUnsupported = errors.New("unsupported on this platform")

type dirInfo struct {
	Dev, Ino uint64
	UID      uint32
}

func plainName(n string) bool                       { return false }
func openPlainDir(string) (int, error)              { return -1, errUnsupported }
func openChain(string, string) (int, error)         { return -1, errUnsupported }
func closeFd(int)                                   {}
func fstatDir(int) (dirInfo, error)                 { return dirInfo{}, errUnsupported }
func statChain(string, string) (dirInfo, error)     { return dirInfo{}, errUnsupported }
func statRegularAt(int, string) (uint32, error)     { return 0, errUnsupported }
func readFileAt(int, string, int64) ([]byte, error) { return nil, errUnsupported }
func existsAt(int, string) (bool, error)            { return false, errUnsupported }
func listDirAt(int) ([]string, error)               { return nil, errUnsupported }
func renameAt(int, string, int, string) error       { return errUnsupported }
