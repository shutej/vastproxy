package backend

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	sshlib "github.com/blacknon/go-sshlib"
	"golang.org/x/crypto/ssh"
)

// Tunnel abstracts SSH tunnel operations so backends can be tested with mocks.
type Tunnel interface {
	LocalAddr() string
	RunCommand(command string) (string, error)
	Close()
}

// TunnelFactory creates a Tunnel. The default uses real SSH via NewSSHTunnel.
type TunnelFactory func(publicIP string, directSSHPort int, sshHost string, sshPort int, keyPath string, remotePort int) (Tunnel, error)

// SSHTunnel manages an SSH connection and local port forward.
type SSHTunnel struct {
	conn      *sshlib.Connect
	localAddr string // "127.0.0.1:<port>" — assigned after Start
}

// Verify SSHTunnel implements Tunnel at compile time.
var _ Tunnel = (*SSHTunnel)(nil)

// NewSSHTunnel creates an SSH connection and establishes a local port forward.
// It tries direct SSH first (publicIP:directPort), then falls back to
// indirect SSH (sshHost:sshPort).
func NewSSHTunnel(publicIP string, directSSHPort int, sshHost string, sshPort int, keyPath string, remotePort int) (Tunnel, error) {
	conn := &sshlib.Connect{}
	conn.HostKeyCallback = ssh.InsecureIgnoreHostKey()

	auth, err := buildAuthMethods(keyPath)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}

	// Try direct SSH first.
	var connected bool
	if publicIP != "" && directSSHPort != 0 {
		if err := conn.CreateClient(publicIP, fmt.Sprintf("%d", directSSHPort), "root", auth); err == nil {
			connected = true
		} else {
			log.Printf("ssh: direct connect to %s:%d failed: %v", publicIP, directSSHPort, err)
		}
	}

	// Fallback to indirect SSH.
	if !connected && sshHost != "" {
		if err := conn.CreateClient(sshHost, fmt.Sprintf("%d", sshPort), "root", auth); err != nil {
			return nil, fmt.Errorf("ssh connect to %s:%d: %w", sshHost, sshPort, err)
		}
		connected = true
	}

	if !connected {
		return nil, fmt.Errorf("no SSH endpoints available")
	}

	// Verify the client was actually created (go-sshlib may leave it nil).
	if conn.Client == nil {
		return nil, fmt.Errorf("ssh client is nil after connect")
	}

	// Find a free local port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		conn.Client.Close()
		return nil, fmt.Errorf("find free port: %w", err)
	}
	localAddr := ln.Addr().String()
	ln.Close()

	tunnel := &SSHTunnel{
		conn:      conn,
		localAddr: localAddr,
	}

	// Start the local port forward in the background.
	// Recover from panics in go-sshlib (it can nil-deref on failed connections).
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", remotePort)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ssh: tunnel panic (recovered): %v", r)
			}
		}()
		if err := conn.TCPLocalForward(localAddr, remoteAddr); err != nil {
			log.Printf("ssh: tunnel to %s exited: %v", remoteAddr, err)
		}
	}()

	return tunnel, nil
}

// LocalAddr returns the local address of the tunnel (e.g., "127.0.0.1:54321").
func (t *SSHTunnel) LocalAddr() string {
	return t.localAddr
}

// RunCommand executes a command over SSH and returns stdout.
func (t *SSHTunnel) RunCommand(command string) (string, error) {
	if t.conn == nil || t.conn.Client == nil {
		return "", fmt.Errorf("ssh not connected")
	}

	session, err := t.conn.Client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	var stdout bytes.Buffer
	session.Stdout = &stdout
	if err := session.Run(command); err != nil {
		return stdout.String(), fmt.Errorf("run command: %w", err)
	}
	return stdout.String(), nil
}

// Close tears down the SSH connection.
func (t *SSHTunnel) Close() {
	if t.conn != nil && t.conn.Client != nil {
		t.conn.Client.Close()
	}
}

func buildAuthMethods(keyPath string) ([]ssh.AuthMethod, error) {
	// Expand ~ in path.
	if keyPath != "" && keyPath[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			keyPath = filepath.Join(home, keyPath[1:])
		}
	}

	var methods []ssh.AuthMethod

	// Try specified key file.
	if keyPath != "" {
		if auth, err := sshlib.CreateAuthMethodPublicKey(keyPath, ""); err == nil {
			methods = append(methods, auth)
		}
	}

	// Try default keys.
	home, _ := os.UserHomeDir()
	for _, name := range []string{"id_rsa", "id_ed25519", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		if p == keyPath {
			continue
		}
		if auth, err := sshlib.CreateAuthMethodPublicKey(p, ""); err == nil {
			methods = append(methods, auth)
		}
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH keys available")
	}
	return methods, nil
}
