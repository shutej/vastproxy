package backend

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	sshlib "github.com/blacknon/go-sshlib"
	"golang.org/x/crypto/ssh"
)

// Tunnel abstracts SSH tunnel operations so backends can be tested with mocks.
type Tunnel interface {
	LocalAddr() string
	RunCommand(command string) (string, error)
	Close()
	IsDirect() bool // true if connected via direct SSH (publicIP:port)
}

// TunnelFactory creates a Tunnel. The default uses real SSH via NewSSHTunnel.
type TunnelFactory func(publicIP string, directSSHPort int, sshHost string, sshPort int, keyPath string, remotePort int) (Tunnel, error)

// SSHTunnel manages an SSH connection and local port forward.
type SSHTunnel struct {
	conn      *sshlib.Connect
	listener  net.Listener
	localAddr string // "127.0.0.1:<port>" — assigned after Start
	isDirect  bool   // true if connected via direct SSH
}

// Verify SSHTunnel implements Tunnel at compile time.
var _ Tunnel = (*SSHTunnel)(nil)

// NewSSHTunnel creates an SSH connection and establishes a local port forward.
// It tries proxy SSH first (sshHost:sshPort) as it is more reliable, then
// falls back to direct SSH (publicIP:directPort).
func NewSSHTunnel(publicIP string, directSSHPort int, sshHost string, sshPort int, keyPath string, remotePort int) (Tunnel, error) {
	conn := &sshlib.Connect{}
	conn.HostKeyCallback = ssh.InsecureIgnoreHostKey()

	auth, err := buildAuthMethods(keyPath)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}

	// Try proxy SSH first (more reliable).
	var connected bool
	var isDirect bool
	if sshHost != "" {
		if err := conn.CreateClient(sshHost, fmt.Sprintf("%d", sshPort), "root", auth); err == nil {
			connected = true
		} else {
			log.Printf("ssh: proxy connect to %s:%d failed: %v", sshHost, sshPort, err)
		}
	}

	// Fallback to direct SSH.
	if !connected && publicIP != "" && directSSHPort != 0 {
		if err := conn.CreateClient(publicIP, fmt.Sprintf("%d", directSSHPort), "root", auth); err != nil {
			return nil, fmt.Errorf("ssh direct connect to %s:%d: %w", publicIP, directSSHPort, err)
		}
		connected = true
		isDirect = true
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
		isDirect:  isDirect,
	}

	// Start the local port forward in the background.
	// We use our own forwarder instead of go-sshlib's TCPLocalForward
	// because the library ignores Dial errors and passes nil to io.Copy,
	// causing unrecoverable panics in child goroutines.
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", remotePort)
	ln2, err := net.Listen("tcp", localAddr)
	if err != nil {
		conn.Client.Close()
		return nil, fmt.Errorf("listen %s: %w", localAddr, err)
	}
	tunnel.listener = ln2

	go func() {
		for {
			local, err := ln2.Accept()
			if err != nil {
				return // listener closed
			}
			remote, err := conn.Client.Dial("tcp", remoteAddr)
			if err != nil {
				log.Printf("ssh: dial remote %s: %v", remoteAddr, err)
				local.Close()
				continue
			}
			go forward(local, remote)
		}
	}()

	return tunnel, nil
}

// forward copies data between two connections and closes both when done.
func forward(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(b, a)
		wg.Done()
	}()
	go func() {
		io.Copy(a, b)
		wg.Done()
	}()
	wg.Wait()
	a.Close()
	b.Close()
}

// LocalAddr returns the local address of the tunnel (e.g., "127.0.0.1:54321").
func (t *SSHTunnel) LocalAddr() string {
	return t.localAddr
}

// IsDirect returns true if the tunnel connected via direct SSH (publicIP:port)
// rather than the vast.ai proxy SSH endpoint.
func (t *SSHTunnel) IsDirect() bool {
	return t.isDirect
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

// Close tears down the SSH connection and stops the local listener.
func (t *SSHTunnel) Close() {
	if t.listener != nil {
		t.listener.Close()
	}
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
