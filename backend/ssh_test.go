package backend

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHServer spins up an in-process SSH server for testing.
// It accepts one connection, handles "exec" requests by running a callback,
// and forwards "direct-tcpip" channels to a local echo server.
type testSSHServer struct {
	ln         net.Listener
	addr       string
	config     *ssh.ServerConfig
	execOutput string // what to return for exec requests
	execErr    bool   // if true, exec returns exit status 1
}

func newTestSSHServer(t *testing.T, clientPubKey ssh.PublicKey) *testSSHServer {
	t.Helper()

	// Generate a host key.
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	config := &ssh.ServerConfig{}
	if clientPubKey != nil {
		config.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientPubKey.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("unknown key")
		}
	} else {
		config.NoClientAuth = true
	}
	config.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &testSSHServer{
		ln:     ln,
		addr:   ln.Addr().String(),
		config: config,
	}

	return s
}

// serve accepts a single SSH connection and handles its channels.
func (s *testSSHServer) serve(t *testing.T) {
	t.Helper()
	go func() {
		tcpConn, err := s.ln.Accept()
		if err != nil {
			return
		}
		sconn, chans, reqs, err := ssh.NewServerConn(tcpConn, s.config)
		if err != nil {
			tcpConn.Close()
			return
		}
		go ssh.DiscardRequests(reqs)

		for newCh := range chans {
			switch newCh.ChannelType() {
			case "session":
				ch, requests, err := newCh.Accept()
				if err != nil {
					continue
				}
				go s.handleSession(ch, requests)
			case "direct-tcpip":
				var payload struct {
					DestAddr   string
					DestPort   uint32
					OriginAddr string
					OriginPort uint32
				}
				if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
					newCh.Reject(ssh.ConnectionFailed, "bad payload")
					continue
				}
				ch, _, err := newCh.Accept()
				if err != nil {
					continue
				}
				go s.handleDirectTCPIP(ch, fmt.Sprintf("%s:%d", payload.DestAddr, payload.DestPort))
			default:
				newCh.Reject(ssh.UnknownChannelType, "unsupported")
			}
		}
		sconn.Close()
	}()
}

func (s *testSSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			if req.WantReply {
				req.Reply(true, nil)
			}
			ch.Write([]byte(s.execOutput))
			if s.execErr {
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
			} else {
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			}
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *testSSHServer) handleDirectTCPIP(ch ssh.Channel, addr string) {
	defer ch.Close()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	defer conn.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(conn, ch); wg.Done() }()
	go func() { io.Copy(ch, conn); wg.Done() }()
	wg.Wait()
}

func (s *testSSHServer) close() {
	s.ln.Close()
}

// writeTestKey writes an ed25519 private key to a temp file and returns
// the path and corresponding ssh.PublicKey.
func writeTestKey(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	// Marshal to OpenSSH format.
	privBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(privBytes), 0600); err != nil {
		t.Fatal(err)
	}

	_ = pub // we return the SSH public key from the signer
	return keyPath, signer.PublicKey()
}

// --- Trivial struct tests ---

func TestSSHTunnelLocalAddr(t *testing.T) {
	tun := &SSHTunnel{localAddr: "127.0.0.1:12345"}
	if got := tun.LocalAddr(); got != "127.0.0.1:12345" {
		t.Errorf("LocalAddr() = %q, want 127.0.0.1:12345", got)
	}
}

func TestSSHTunnelIsDirect(t *testing.T) {
	tun := &SSHTunnel{isDirect: false}
	if tun.IsDirect() {
		t.Error("IsDirect() = true, want false")
	}
	tun2 := &SSHTunnel{isDirect: true}
	if !tun2.IsDirect() {
		t.Error("IsDirect() = false, want true")
	}
}

func TestSSHTunnelCloseNilFields(t *testing.T) {
	// Close should not panic with nil conn and listener.
	tun := &SSHTunnel{}
	tun.Close() // must not panic
}

func TestSSHTunnelCloseListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tun := &SSHTunnel{listener: ln}
	tun.Close()

	// Verify listener is actually closed.
	_, err = ln.Accept()
	if err == nil {
		t.Error("expected error from closed listener")
	}
}

func TestSSHTunnelRunCommandNoClient(t *testing.T) {
	tun := &SSHTunnel{}
	_, err := tun.RunCommand("echo hello")
	if err == nil {
		t.Fatal("expected error with nil conn")
	}
}

func TestSetBaseURL(t *testing.T) {
	inst := testInstance(1)
	be := NewBackend(inst, "", nil)
	be.SetBaseURL("http://127.0.0.1:9999/v1")
	if got := be.BaseURL(); got != "http://127.0.0.1:9999/v1" {
		t.Errorf("BaseURL() = %q after SetBaseURL", got)
	}
}

// --- buildAuthMethods tests ---

func TestBuildAuthMethodsWithValidKey(t *testing.T) {
	keyPath, _ := writeTestKey(t)

	methods, err := buildAuthMethods(keyPath)
	if err != nil {
		t.Fatalf("buildAuthMethods() error: %v", err)
	}
	if len(methods) == 0 {
		t.Fatal("expected at least one auth method")
	}
}

func TestBuildAuthMethodsNoKeys(t *testing.T) {
	// Use a nonexistent key path and override HOME to prevent picking up real keys.
	t.Setenv("HOME", t.TempDir())
	_, err := buildAuthMethods("/nonexistent/path/id_rsa")
	if err == nil {
		t.Fatal("expected error when no keys are available")
	}
}

// --- In-process SSH server tests ---

func TestNewSSHTunnelProxyConnect(t *testing.T) {
	keyPath, pubKey := writeTestKey(t)
	srv := newTestSSHServer(t, pubKey)
	defer srv.close()
	srv.serve(t)

	host, port := splitHostPort(t, srv.addr)

	tunnel, err := NewSSHTunnel("", 0, host, port, keyPath, 8080)
	if err != nil {
		t.Fatalf("NewSSHTunnel() error: %v", err)
	}
	defer tunnel.Close()

	if tunnel.LocalAddr() == "" {
		t.Error("LocalAddr() is empty")
	}
	if tunnel.IsDirect() {
		t.Error("IsDirect() should be false for proxy connect")
	}
}

func TestNewSSHTunnelDirectConnect(t *testing.T) {
	keyPath, pubKey := writeTestKey(t)
	srv := newTestSSHServer(t, pubKey)
	defer srv.close()
	srv.serve(t)

	host, port := splitHostPort(t, srv.addr)

	// Empty sshHost forces direct connect fallback.
	tunnel, err := NewSSHTunnel(host, port, "", 0, keyPath, 8080)
	if err != nil {
		t.Fatalf("NewSSHTunnel() error: %v", err)
	}
	defer tunnel.Close()

	if !tunnel.IsDirect() {
		t.Error("IsDirect() should be true for direct connect")
	}
}

func TestNewSSHTunnelNoEndpoints(t *testing.T) {
	keyPath, _ := writeTestKey(t)
	_, err := NewSSHTunnel("", 0, "", 0, keyPath, 8080)
	if err == nil {
		t.Fatal("expected error with no endpoints")
	}
}

func TestNewSSHTunnelProxyFailFallbackDirect(t *testing.T) {
	keyPath, pubKey := writeTestKey(t)
	srv := newTestSSHServer(t, pubKey)
	defer srv.close()
	srv.serve(t)

	host, port := splitHostPort(t, srv.addr)

	// Provide a bad proxy host so it fails, then falls back to direct.
	tunnel, err := NewSSHTunnel(host, port, "127.0.0.1", 1, keyPath, 8080)
	if err != nil {
		t.Fatalf("NewSSHTunnel() error: %v", err)
	}
	defer tunnel.Close()

	if !tunnel.IsDirect() {
		t.Error("should have fallen back to direct")
	}
}

func TestSSHTunnelRunCommand(t *testing.T) {
	keyPath, pubKey := writeTestKey(t)
	srv := newTestSSHServer(t, pubKey)
	defer srv.close()
	srv.execOutput = "hello world\n"
	srv.serve(t)

	host, port := splitHostPort(t, srv.addr)

	tunnel, err := NewSSHTunnel("", 0, host, port, keyPath, 8080)
	if err != nil {
		t.Fatalf("NewSSHTunnel() error: %v", err)
	}
	defer tunnel.Close()

	out, err := tunnel.RunCommand("echo hello world")
	if err != nil {
		t.Fatalf("RunCommand() error: %v", err)
	}
	if out != "hello world\n" {
		t.Errorf("RunCommand() = %q, want %q", out, "hello world\n")
	}
}

func TestSSHTunnelRunCommandError(t *testing.T) {
	keyPath, pubKey := writeTestKey(t)
	srv := newTestSSHServer(t, pubKey)
	defer srv.close()
	srv.execOutput = "something went wrong"
	srv.execErr = true
	srv.serve(t)

	host, port := splitHostPort(t, srv.addr)

	tunnel, err := NewSSHTunnel("", 0, host, port, keyPath, 8080)
	if err != nil {
		t.Fatalf("NewSSHTunnel() error: %v", err)
	}
	defer tunnel.Close()

	out, err := tunnel.RunCommand("false")
	if err == nil {
		t.Fatal("expected error from failed command")
	}
	if out != "something went wrong" {
		t.Errorf("RunCommand() output = %q, want partial output", out)
	}
}

func TestSSHTunnelPortForward(t *testing.T) {
	keyPath, pubKey := writeTestKey(t)

	// Start a TCP echo server that the SSH tunnel will forward to.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()

	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(conn, conn)
				conn.Close()
			}()
		}
	}()

	_, echoPort := splitHostPort(t, echoLn.Addr().String())

	srv := newTestSSHServer(t, pubKey)
	defer srv.close()
	srv.serve(t)

	host, port := splitHostPort(t, srv.addr)

	tunnel, err := NewSSHTunnel("", 0, host, port, keyPath, echoPort)
	if err != nil {
		t.Fatalf("NewSSHTunnel() error: %v", err)
	}
	defer tunnel.Close()

	// Connect through the tunnel and verify data round-trips.
	conn, err := net.Dial("tcp", tunnel.LocalAddr())
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()

	msg := "hello through tunnel"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read back exactly the same number of bytes we wrote.
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("got %q, want %q", string(buf), msg)
	}
}

func TestForwardFunction(t *testing.T) {
	// Test the forward() function directly using two TCP connections via a
	// listener pair (net.Pipe doesn't propagate half-close properly).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept one connection in background.
	type connResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan connResult, 1)
	go func() {
		c, err := ln.Accept()
		ch <- connResult{c, err}
	}()

	clientA, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	res := <-ch
	if res.err != nil {
		t.Fatal(res.err)
	}
	serverA := res.conn

	// Create a second pair.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()

	go func() {
		c, err := ln2.Accept()
		ch <- connResult{c, err}
	}()

	clientB, err := net.Dial("tcp", ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	res = <-ch
	if res.err != nil {
		t.Fatal(res.err)
	}
	serverB := res.conn

	// forward copies data between serverA and serverB.
	done := make(chan struct{})
	go func() {
		forward(serverA, serverB)
		close(done)
	}()

	// Write on clientA → should come out clientB.
	msg := "test data"
	clientA.Write([]byte(msg))
	clientA.Close() // EOF on serverA's read side

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(clientB, buf); err != nil {
		t.Fatalf("read from clientB: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("forward: got %q, want %q", string(buf), msg)
	}

	// Close clientB to let the reverse copy finish.
	clientB.Close()
	<-done
}

// --- helpers ---

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}
