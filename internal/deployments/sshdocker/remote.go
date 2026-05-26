// Package sshdocker implements iplane's deployment executor for v0.1:
// SSH into a provisioned instance and run the engine container via
// docker CLI commands over the SSH session. One concrete executor,
// no Deployer interface yet -- the design doc locks that in until
// v0.3 brings a second concrete deployment strategy worth abstracting
// over.
//
// Three layers:
//
//   - remote.go   : RemoteRunner -- abstract "run a command on the
//                   remote box, get stdout/stderr/exit". SSH-backed
//                   in production, fake in tests.
//   - docker.go   : typed wrappers over the docker CLI on the remote
//                   side (Inspect / Pull / Run / Stop / Remove /
//                   Health). Composes RemoteRunner.Run().
//   - executor.go : state-machine orchestration. Deploy and Destroy
//                   walk a Deployment through PENDING -> STARTING ->
//                   CONFIGURING -> RUNNING (or FAILED). Composes the
//                   docker wrappers.
//
// Issue 25 (deferred) tracks the Runner abstraction: today the
// Service spawns an executor goroutine directly, which is fine for
// v0.1's single-process model. Later versions need throttling /
// restartability / distribution; Runner is where that lands.
package sshdocker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// RemoteRunner abstracts "run a command on the remote box". The
// production impl dials SSH with the operator's iplane key; tests
// substitute a fake that responds to known commands with canned
// output. The interface stays narrow: one synchronous method, no
// streaming -- the docker commands we issue are short-lived.
//
// stdout / stderr are returned even on non-zero exit so callers can
// surface the remote's error messages (e.g., docker's "image not
// found" goes to stderr; we want to propagate it to the operator).
type RemoteRunner interface {
	Run(ctx context.Context, cmd string) (stdout, stderr []byte, exitCode int, err error)

	// Close releases any underlying connection. Idempotent.
	Close() error
}

// SSHRunner is the production RemoteRunner. Holds a persistent SSH
// connection so multiple commands (inspect, pull, run, health) share
// one channel instead of re-handshaking per command.
type SSHRunner struct {
	client *ssh.Client
}

// SSHConfig is the connection info needed to dial. host:port is the
// instance's SSH endpoint (from Instance.ssh on the proto); user is
// typically "root" for RunPod pods; privateKey is the operator's
// iplane Ed25519 private bytes.
type SSHConfig struct {
	Host       string
	Port       int
	User       string
	PrivateKey ed25519.PrivateKey

	// HostKeyCallback is required for production -- defaults to
	// ssh.InsecureIgnoreHostKey() if unset, which is acceptable for
	// v0.1 (provider's pod identity is established by the provider's
	// own management plane; SSH host key TOFU is a defense layer
	// against MITM that the v0.2+ chapter narrative may revisit).
	HostKeyCallback ssh.HostKeyCallback

	// Timeout for the initial Dial. Defaults to 30s.
	DialTimeout time.Duration
}

// NewSSHRunner dials the instance and returns a runner ready to
// execute commands. Caller must Close() when done.
func NewSSHRunner(ctx context.Context, cfg SSHConfig) (*SSHRunner, error) {
	if cfg.Host == "" {
		return nil, errors.New("sshdocker: SSH host is required (instance has no ssh.host -- deployment requires an SSH-reachable instance)")
	}
	if cfg.User == "" {
		cfg.User = "root"
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 30 * time.Second
	}

	signer, err := ssh.NewSignerFromKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("sshdocker: build signer from ed25519 key: %w", err)
	}

	hostKeyCallback := cfg.HostKeyCallback
	if hostKeyCallback == nil {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	}

	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         cfg.DialTimeout,
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))

	// Dial via a context-aware net.Dialer so the operator's Ctrl-C
	// can interrupt a hung handshake. x/crypto/ssh's Dial does not
	// accept a context directly.
	d := net.Dialer{Timeout: cfg.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshdocker: tcp dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sshdocker: ssh handshake %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	return &SSHRunner{client: client}, nil
}

// Run executes one command on the remote box. Each call opens a new
// SSH session (cheap on an established connection); we don't reuse
// sessions because docker commands need clean stdin/stdout streams.
func (r *SSHRunner) Run(ctx context.Context, cmd string) (stdout, stderr []byte, exitCode int, err error) {
	session, err := r.client.NewSession()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("sshdocker: open session: %w", err)
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf

	// Run with context cancellation: if ctx fires, close the session
	// to unblock the Wait. x/crypto/ssh.Session has no native ctx
	// integration; the close-on-cancel pattern is the standard
	// workaround.
	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case runErr := <-done:
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			return outBuf.Bytes(), errBuf.Bytes(), exitErr.ExitStatus(), nil
		}
		if runErr != nil {
			return outBuf.Bytes(), errBuf.Bytes(), -1, fmt.Errorf("sshdocker: run %q: %w", cmd, runErr)
		}
		return outBuf.Bytes(), errBuf.Bytes(), 0, nil
	case <-ctx.Done():
		_ = session.Close()
		<-done // wait for the goroutine to return so it does not leak
		return outBuf.Bytes(), errBuf.Bytes(), -1, ctx.Err()
	}
}

// Close releases the underlying SSH connection.
func (r *SSHRunner) Close() error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}
