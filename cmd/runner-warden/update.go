package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
)

const (
	releaseRepo = "kylepantall/runner-warden"
	maxArchive  = 200 << 20
)

func updateCmd() *cobra.Command {
	var target string
	var check, noRestart, requireSig bool
	c := &cobra.Command{
		Use:   "update",
		Short: "Update runner-warden to the latest release (verifies checksum and, with cosign, the signature)",
		Long: "Downloads the release for this OS/arch, verifies it against the release's checksums.txt " +
			"(and its cosign bundle when cosign is installed), replaces this binary and the systemd " +
			"unit atomically, then restarts every configured instance. A restart drains: running jobs " +
			"keep going and are adopted by the new version.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			tag := target
			if tag == "" {
				t, err := latestTag(ctx)
				if err != nil {
					return err
				}
				tag = t
			}
			want := strings.TrimPrefix(tag, "v")
			if want == strings.TrimPrefix(version, "v") {
				fmt.Fprintf(out, "runner-warden %s is up to date\n", version)
				return nil
			}
			fmt.Fprintf(out, "update available: %s → %s\n", version, want)
			if check {
				return nil
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			bin, unit, err := fetchRelease(ctx, want, requireSig, out)
			if err != nil {
				return err
			}
			if err := replaceFile(exe, bin, 0o755); err != nil {
				return fmt.Errorf("replace %s (run with sudo?): %w", exe, err)
			}
			fmt.Fprintf(out, "✅ installed %s %s\n", exe, want)
			if unit != nil && runtime.GOOS == "linux" {
				const unitPath = "/etc/systemd/system/runner-warden@.service"
				if cur, err := os.ReadFile(unitPath); err == nil && !bytes.Equal(cur, unit) {
					if err := replaceFile(unitPath, unit, 0o644); err != nil {
						return err
					}
					_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
					fmt.Fprintln(out, "✅ updated systemd unit")
				}
			}
			if noRestart || runtime.GOOS != "linux" {
				return nil
			}
			insts, _ := filepath.Glob(filepath.Join(config.EtcDir, "*", "config.toml"))
			for _, p := range insts {
				inst := filepath.Base(filepath.Dir(p))
				unit := "runner-warden@" + inst
				if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run(); err != nil {
					continue
				}
				if out2, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput(); err != nil {
					return fmt.Errorf("restart %s: %w: %s", unit, err, out2)
				}
				fmt.Fprintf(out, "✅ restarted %s\n", unit)
			}
			return nil
		},
	}
	c.Flags().StringVar(&target, "version", "", "install this version (e.g. v0.1.1) instead of the latest")
	c.Flags().BoolVar(&check, "check", false, "only report whether an update is available")
	c.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart running instances")
	c.Flags().BoolVar(&requireSig, "require-signature", false, "fail unless cosign verifies the release signature")
	return c
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func latestTag(ctx context.Context) (string, error) {
	b, err := get(ctx, "https://api.github.com/repos/"+releaseRepo+"/releases/latest", 1<<20)
	if err != nil {
		return "", err
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.TagName == "" {
		return "", errors.New("could not read the latest release")
	}
	return r.TagName, nil
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// fetchRelease downloads and verifies the archive for this platform and
// returns the binary and the systemd unit it ships.
func fetchRelease(ctx context.Context, ver string, requireSig bool, out io.Writer) (bin, unit []byte, err error) {
	name := fmt.Sprintf("runner-warden_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("https://github.com/%s/releases/download/v%s/", releaseRepo, ver)
	sums, err := get(ctx, base+"checksums.txt", 1<<20)
	if err != nil {
		return nil, nil, err
	}
	if err := verifySignature(ctx, base, sums, requireSig, out); err != nil {
		return nil, nil, err
	}
	wantSum := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			wantSum = f[0]
		}
	}
	if wantSum == "" {
		return nil, nil, fmt.Errorf("no checksum for %s (unsupported platform?)", name)
	}
	archive, err := get(ctx, base+name, maxArchive)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != wantSum {
		return nil, nil, errors.New("checksum mismatch")
	}
	fmt.Fprintln(out, "✅ checksum verified")
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch filepath.ToSlash(h.Name) {
		case "runner-warden":
			if bin, err = io.ReadAll(io.LimitReader(tr, maxArchive)); err != nil {
				return nil, nil, err
			}
		case "packaging/systemd/runner-warden@.service":
			if unit, err = io.ReadAll(io.LimitReader(tr, 1<<20)); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(bin) == 0 {
		return nil, nil, errors.New("binary not found in the release archive")
	}
	return bin, unit, nil
}

// verifySignature checks checksums.txt against its keyless cosign bundle when
// cosign is installed (or required).
func verifySignature(ctx context.Context, base string, sums []byte, require bool, out io.Writer) error {
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		if require {
			return errors.New("cosign not installed and --require-signature set")
		}
		fmt.Fprintln(out, "⚠️  cosign not installed: checksum verified, signature not checked")
		return nil
	}
	bundle, err := get(ctx, base+"checksums.txt.sigstore.json", 1<<20)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "runner-warden-update-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sumsPath, bundlePath := filepath.Join(dir, "checksums.txt"), filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return err
	}
	c := exec.CommandContext(ctx, cosign, "verify-blob", "--bundle", bundlePath, //nolint:gosec // fixed binary from PATH, our own temp files
		"--certificate-identity-regexp", `^https://github.com/`+releaseRepo+`/\.github/workflows/release\.yml@refs/tags/v`,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", sumsPath)
	if b, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("signature verification failed: %s", strings.TrimSpace(string(b)))
	}
	fmt.Fprintln(out, "✅ signature verified")
	return nil
}

// replaceFile writes next to dst and renames over it, so a running binary
// keeps its old inode and nothing sees a half-written file.
func replaceFile(dst string, data []byte, mode os.FileMode) error {
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
