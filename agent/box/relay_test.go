package box

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmitriyb/faber/security"
	"golang.org/x/crypto/ssh/agent"
)

// strictHost is the host's end of the relay's stream with a gate in front
// of the agent server: it reads one request off the relay's stdout (a real
// pipe, buffered like the exec's), checks that no further bytes are already
// waiting — a relay that let two clients' requests onto the stream before
// the first reply would have them there — then serves it and writes the
// reply to the relay's stdin. violations counts requests found waiting; the
// host keeps serving after one, so the test ends and reports it.
type strictHost struct {
	violations atomic.Int32
}

// run gates fromRelay into ServeAgent until fromRelay ends.
func (h *strictHost) run(fromRelay *os.File, toRelay *os.File, ring agent.Agent) {
	toServer, serverIn := io.Pipe()
	serverOut, fromServer := io.Pipe()
	go agent.ServeAgent(ring, struct {
		io.Reader
		io.Writer
	}{toServer, fromServer})
	defer serverIn.Close()
	buffered := bufio.NewReader(fromRelay)
	for {
		msg, err := readAgentMessage(buffered)
		if err != nil {
			return
		}
		// Anything readable now arrived before this request's reply. The
		// peek keeps the byte buffered, so the stream stays whole.
		fromRelay.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
		if _, err := buffered.Peek(1); err == nil {
			h.violations.Add(1)
		}
		fromRelay.SetReadDeadline(time.Time{})
		if _, err := serverIn.Write(msg); err != nil {
			return
		}
		reply, err := readAgentMessage(serverOut)
		if err != nil {
			return
		}
		if _, err := toRelay.Write(reply); err != nil {
			return
		}
	}
}

// startRelay runs Relay on a scratch socket against a strict in-process
// host over real pipes and returns the socket, the host, and a stop that
// closes the host's side and waits for the relay to end with its error.
func startRelay(t *testing.T) (sock string, host *strictHost, stop func() error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "faber-rt") // short: unix socket limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "agent.sock")
	ring := agent.NewKeyring()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := ring.Add(agent.AddedKey{PrivateKey: priv, Comment: "role"}); err != nil {
		t.Fatal(err)
	}
	relayIn, hostOut, err := os.Pipe() // host -> relay stdin
	if err != nil {
		t.Fatal(err)
	}
	hostIn, relayOut, err := os.Pipe() // relay stdout -> host
	if err != nil {
		t.Fatal(err)
	}
	host = &strictHost{}
	go host.run(hostIn, hostOut, ring)
	done := make(chan error, 1)
	go func() {
		done <- Relay(context.Background(), &BoxEnv{AgentSocket: sock}, relayIn, relayOut, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return sock, host, func() error {
		hostOut.Close() // the relay's stdin ends: an orderly end
		err := <-done
		hostIn.Close()
		relayIn.Close()
		relayOut.Close()
		return err
	}
}

func dialAgent(t *testing.T, sock string) (agent.ExtendedAgent, func()) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	return agent.NewClient(conn), func() { conn.Close() }
}

// Verifies 93ba0858d75f (test scenario 23): the relay serves concurrent
// clients over one request–reply stream — the strict host never finds a
// second request waiting before it has replied to the first, and each
// client gets the signature over its own data — a client with a malformed
// length is disconnected alone, and the host's side closing ends the relay
// cleanly.
func TestRelayPumpsOneExchangeAtATime(t *testing.T) {
	sock, host, stop := startRelay(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, done := dialAgent(t, sock)
			defer done()
			for j := 0; j < 10; j++ {
				keys, err := client.List()
				if err != nil || len(keys) != 1 || keys[0].Comment != "role" {
					t.Errorf("List = %v, %v", keys, err)
					return
				}
				data := []byte(fmt.Sprintf("client %d message %d", i, j))
				sig, err := client.Sign(keys[0], data)
				if err != nil {
					t.Errorf("Sign: %v", err)
					return
				}
				if err := keys[0].Verify(data, sig); err != nil {
					t.Errorf("client %d got a signature over someone else's data: %v", i, err)
				}
			}
		}()
	}
	wg.Wait()
	if got := host.violations.Load(); got != 0 {
		t.Fatalf("the host found a request waiting behind an unanswered one %d times: the relay interleaved clients", got)
	}

	// A malformed client is dropped alone: the others keep working.
	bad, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	bad.Write([]byte{0xff, 0xff, 0xff, 0xff})
	bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bad.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("malformed client: read = %v, want EOF (disconnected)", err)
	}
	client, done := dialAgent(t, sock)
	if keys, err := client.List(); err != nil || len(keys) != 1 {
		t.Fatalf("after the malformed client, List = %v, %v", keys, err)
	}
	done()

	if err := stop(); err != nil {
		t.Fatalf("relay ended with %v after the host closed its side", err)
	}
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("the socket still accepts after the relay ended")
	}
}

// Verifies 93ba0858d75f (test scenario 23): the host's side closing ends
// the relay even while a client sits connected and idle — the client is
// cut off (its agent is gone) and Relay returns.
func TestRelayEndsWithTheHostWhileClientsAreConnected(t *testing.T) {
	sock, _, stop := startRelay(t)
	idle, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	ended := make(chan error, 1)
	go func() { ended <- stop() }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("relay ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not end while a client was connected")
	}
	idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("idle client: read = %v, want EOF", err)
	}
}

// Verifies 93ba0858d75f: a relay with no SSH_AUTH_SOCK to serve is refused.
func TestRelayNeedsTheSocketVariable(t *testing.T) {
	err := Relay(context.Background(), &BoxEnv{}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), security.EnvSSHAuthSock) {
		t.Fatalf("err = %v, want the variable named", err)
	}
}
