package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sshState holds the live SSH connection. The SSH password / key passphrase is
// only kept in memory for this session and never written to disk.
type sshState struct {
	mu       sync.Mutex
	client   *ssh.Client
	user     string
	password string // used for sudo when the SSH user is not root
}

var sshS = &sshState{}

var errNotConnected = errors.New("not connected to the server over SSH: open Setup and click Connect (your SSH password is only kept in memory for this session)")

func (s *sshState) get() (*ssh.Client, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil, "", "", errNotConnected
	}
	return s.client, s.user, s.password, nil
}

func (s *sshState) connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client != nil
}

func (s *sshState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		_ = s.client.Close()
	}
	s.client = nil
	s.password = ""
}

// connectSSH dials the server. Host key is pinned on first use (TOFU) and
// verified on every later connection.
func connectSSH(password, passphrase string) (fingerprint string, firstTime bool, err error) {
	c := snapshotConfig()
	if c.ServerIP == "" || c.SSHUser == "" {
		return "", false, errors.New("enter the server IP and SSH user first")
	}
	var auths []ssh.AuthMethod
	if c.SSHKeyPath != "" {
		keyBytes, err := os.ReadFile(c.SSHKeyPath)
		if err != nil {
			return "", false, fmt.Errorf("read SSH key: %w", err)
		}
		var signer ssh.Signer
		if passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(keyBytes)
		}
		if err != nil {
			var pm *ssh.PassphraseMissingError
			if errors.As(err, &pm) {
				return "", false, errors.New("this SSH key is protected by a passphrase: enter it in the passphrase field")
			}
			return "", false, fmt.Errorf("parse SSH key (OpenSSH/PEM format expected; PuTTY .ppk files must be converted with PuTTYgen): %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if c.SSHKeyPath == "" && c.ManagedKey != "" {
		// Dilreet's own key (created by "Remember this server" or when
		// Dilreet created the server). Stored encrypted, never as a file.
		if signer, err := ssh.ParsePrivateKey([]byte(secret(c.ManagedKey))); err == nil {
			auths = append(auths, ssh.PublicKeys(signer))
		}
	}
	if password != "" {
		auths = append(auths, ssh.Password(password))
		auths = append(auths, ssh.KeyboardInteractive(func(user, instr string, qs []string, echos []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range qs {
				ans[i] = password
			}
			return ans, nil
		}))
	}
	if len(auths) == 0 {
		return "", false, errors.New("enter the server's root password (or choose an SSH key file)")
	}
	var seenKey ssh.PublicKey
	pinned := strings.TrimSpace(c.HostKey)
	hostKeyCB := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		seenKey = key
		if pinned == "" {
			return nil // first connection: trust and pin below
		}
		want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned))
		if err != nil {
			return fmt.Errorf("stored host key is invalid: %w", err)
		}
		if !bytes.Equal(want.Marshal(), key.Marshal()) {
			return fmt.Errorf("SERVER HOST KEY CHANGED (expected %s, got %s). If you reinstalled the server, click 'Forget host key' in Setup; otherwise someone may be intercepting the connection", ssh.FingerprintSHA256(want), ssh.FingerprintSHA256(key))
		}
		return nil
	}
	conf := &ssh.ClientConfig{
		User:            c.SSHUser,
		Auth:            auths,
		HostKeyCallback: hostKeyCB,
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(c.ServerIP, strconv.Itoa(c.SSHPort))
	client, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		return "", false, fmt.Errorf("SSH connection to %s failed: %w", addr, err)
	}
	if pinned == "" && seenKey != nil {
		firstTime = true
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(seenKey)))
		_ = withConfig(func(c *Config) error { c.HostKey = line; return nil })
	}
	sshS.close()
	sshS.mu.Lock()
	sshS.client = client
	sshS.user = c.SSHUser
	sshS.password = password
	sshS.mu.Unlock()
	go keepAlive(client)
	if seenKey != nil {
		fingerprint = ssh.FingerprintSHA256(seenKey)
	}
	return fingerprint, firstTime, nil
}

func keepAlive(c *ssh.Client) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			sshS.mu.Lock()
			if sshS.client == c {
				sshS.client = nil
			}
			sshS.mu.Unlock()
			return
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// runRemote runs a shell command as root (via sudo if the SSH user is not
// root), streaming stdout+stderr to out.
func runRemote(ctx context.Context, script string, out io.Writer) error {
	client, user, password, err := sshS.get()
	if err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH session: %w", err)
	}
	defer sess.Close()
	sess.Stdout = out
	sess.Stderr = out
	cmd := "bash -c " + shellQuote(script)
	if user != "root" {
		if password != "" {
			cmd = "sudo -S -p '' bash -c " + shellQuote(script)
			sess.Stdin = strings.NewReader(password + "\n")
		} else {
			// Key auth without a password: only passwordless sudo can work.
			cmd = "sudo -n bash -c " + shellQuote(script)
		}
	}
	if err := sess.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var ee *ssh.ExitError
			if errors.As(err, &ee) {
				return fmt.Errorf("remote command exited with status %d", ee.ExitStatus())
			}
		}
		return err
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return ctx.Err()
	}
}

// runRemoteOutput runs a command and returns its combined output.
func runRemoteOutput(ctx context.Context, script string) (string, error) {
	var buf bytes.Buffer
	err := runRemote(ctx, script, &buf)
	return buf.String(), err
}

// uploadFile copies a local file to remotePath over SFTP (as the SSH user).
func uploadFile(localPath, remotePath string, progress func(done, total int64)) error {
	client, _, _, err := sshS.get()
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("start SFTP (is the SFTP subsystem enabled in sshd?): %w", err)
	}
	defer sc.Close()
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	st, _ := src.Stat()
	dst, err := sc.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("create remote file %s: %w", remotePath, err)
	}
	defer dst.Close()
	_ = dst.Chmod(0o600)
	pr := &progressReader{r: src, total: st.Size(), cb: progress}
	if _, err := io.Copy(dst, pr); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return dst.Close()
}

type progressReader struct {
	r     io.Reader
	n     int64
	total int64
	last  time.Time
	cb    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.cb != nil && (time.Since(p.last) > time.Second || err == io.EOF) {
		p.last = time.Now()
		p.cb(p.n, p.total)
	}
	return n, err
}

// dialRemote opens a TCP connection from the server side (used to tunnel the
// Coolify API through SSH so the token never crosses the internet in clear).
func dialRemote(addr string) (net.Conn, error) {
	client, _, _, err := sshS.get()
	if err != nil {
		return nil, err
	}
	return client.Dial("tcp", addr)
}

// uploadSecret writes data to a new 0600 file in the SSH user's home folder
// over SFTP and returns its absolute path. Used so secrets never appear on a
// remote command line (where other processes could see them). The caller's
// remote script must delete the file.
func uploadSecret(data []byte) (string, error) {
	client, _, _, err := sshS.get()
	if err != nil {
		return "", err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return "", fmt.Errorf("start SFTP: %w", err)
	}
	defer sc.Close()
	home, err := sc.Getwd()
	if err != nil || home == "" {
		home = "/tmp"
	}
	p := strings.TrimRight(home, "/") + "/.dilreet-" + randHex(8)
	f, err := sc.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", p, err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		_ = sc.Remove(p)
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = sc.Remove(p)
		return "", err
	}
	return p, f.Close()
}
