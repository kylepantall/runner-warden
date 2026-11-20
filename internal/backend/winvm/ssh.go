package winvm

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Exec runs PowerShell inside a guest. Tests use a fake.
type Exec interface {
	// Run executes a PowerShell script. A non-zero exit is not an error.
	Run(ctx context.Context, script string) (stdout, stderr string, exit int, err error)
	// RunStdin is Run with stdin; the script reads it with [Console]::In.
	// Secrets travel this way so they never sit on a command line.
	RunStdin(ctx context.Context, script, stdin string) (stdout, stderr string, exit int, err error)
}

const (
	defaultMaxOutput = 1 << 20
	// maxEncoded bounds the -EncodedCommand argument; Windows OpenSSH with a
	// PowerShell default shell accepts about 32 KB of command line.
	maxEncoded = 30000
)

// SSHConfig describes one guest endpoint.
type SSHConfig struct {
	Addr        string // host:port, e.g. 127.0.0.1:2222
	User        string
	Signer      ssh.Signer // key auth; nil when Password is used
	Password    string
	KnownHosts  string // TOFU file, created on first connect
	DialTimeout time.Duration
	MaxOutput   int
}

type sshExec struct {
	cfg   SSHConfig
	winPS atomic.Bool // pwsh missing: use powershell.exe
}

var _ Exec = (*sshExec)(nil)

var knownHostsMu sync.Mutex

// NewSSHExec returns an Exec that dials a fresh connection per call.
func NewSSHExec(cfg SSHConfig) Exec {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 15 * time.Second
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = defaultMaxOutput
	}
	return &sshExec{cfg: cfg}
}

// encodePS is the -EncodedCommand form: base64 of UTF-16LE.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func shellCommand(shell, script string) (string, error) {
	enc := encodePS(script)
	if len(enc) > maxEncoded {
		return "", errors.New("script too large for one ssh command")
	}
	return shell + " -NoProfile -NonInteractive -EncodedCommand " + enc, nil
}

func notRecognized(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "is not recognized") || strings.Contains(s, "cannot find") || strings.Contains(s, "command not found")
}

func (x *sshExec) Run(ctx context.Context, script string) (string, string, int, error) {
	return x.RunStdin(ctx, script, "")
}

func (x *sshExec) RunStdin(ctx context.Context, script, stdin string) (string, string, int, error) {
	shells := []string{"pwsh", "powershell.exe"}
	if x.winPS.Load() {
		shells = shells[1:]
	}
	for i, sh := range shells {
		cmd, err := shellCommand(sh, script)
		if err != nil {
			return "", "", -1, err
		}
		out, errs, code, err := x.exec(ctx, cmd, stdin)
		if err != nil {
			return out, errs, code, err
		}
		if i == 0 && len(shells) == 2 && code != 0 && notRecognized(out+errs) {
			x.winPS.Store(true)
			continue
		}
		return out, errs, code, nil
	}
	return "", "", -1, errors.New("no powershell")
}

type capBuf struct {
	buf bytes.Buffer
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (x *sshExec) clientConfig() (*ssh.ClientConfig, error) {
	cc := &ssh.ClientConfig{
		User:            x.cfg.User,
		HostKeyCallback: tofuCallback(x.cfg.KnownHosts),
		Timeout:         x.cfg.DialTimeout,
	}
	switch {
	case x.cfg.Signer != nil:
		cc.Auth = []ssh.AuthMethod{ssh.PublicKeys(x.cfg.Signer)}
	case x.cfg.Password != "":
		cc.Auth = []ssh.AuthMethod{ssh.Password(x.cfg.Password)}
	default:
		return nil, errors.New("ssh: no key or password")
	}
	return cc, nil
}

func (x *sshExec) dial(ctx context.Context) (*ssh.Client, error) {
	cc, err := x.clientConfig()
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Timeout: x.cfg.DialTimeout}).DialContext(ctx, "tcp", x.cfg.Addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(x.cfg.DialTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, x.cfg.Addr, cc)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func (x *sshExec) exec(ctx context.Context, cmd, stdin string) (string, string, int, error) {
	client, err := x.dial(ctx)
	if err != nil {
		return "", "", -1, err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", "", -1, err
	}
	defer sess.Close()
	out, errb := &capBuf{max: x.cfg.MaxOutput}, &capBuf{max: x.cfg.MaxOutput}
	sess.Stdout, sess.Stderr = out, errb
	if stdin != "" {
		sess.Stdin = strings.NewReader(stdin)
	}
	if err := sess.Start(cmd); err != nil {
		return "", "", -1, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	keep := time.NewTicker(30 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = client.Close()
			return out.buf.String(), errb.buf.String(), -1, ctx.Err()
		case <-keep.C:
			_, _, _ = client.SendRequest("keepalive@openssh.com", true, nil)
		case err := <-done:
			var ee *ssh.ExitError
			switch {
			case err == nil:
				return out.buf.String(), errb.buf.String(), 0, nil
			case errors.As(err, &ee):
				return out.buf.String(), errb.buf.String(), ee.ExitStatus(), nil
			}
			return out.buf.String(), errb.buf.String(), -1, err
		}
	}
}

// tofuCallback trusts a host key on first use and pins it afterwards.
func tofuCallback(path string) ssh.HostKeyCallback {
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
		if err != nil {
			return err
		}
		_ = f.Close()
		cb, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = cb(host, remote, key)
		var ke *knownhosts.KeyError
		if err == nil || !errors.As(err, &ke) || len(ke.Want) > 0 {
			return err // trusted, or a changed key: refuse
		}
		af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer af.Close()
		_, err = af.WriteString(knownhosts.Line([]string{host}, key) + "\n")
		return err
	}
}

// EnsureKey loads the ed25519 key at path, generating it (0600, plus a .pub
// file) when missing. It returns the signer and the authorized_keys line.
func EnsureKey(path string) (ssh.Signer, string, error) {
	if b, err := os.ReadFile(path); err == nil {
		s, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, "", fmt.Errorf("parse ssh key: %w", err)
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
		if _, err := os.Stat(path + ".pub"); err != nil {
			_ = os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
		}
		return s, line, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "runner-warden")
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, "", err
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	if err := os.WriteFile(path+".pub", []byte(line+"\n"), 0o644); err != nil {
		return nil, "", err
	}
	return s, line, nil
}
