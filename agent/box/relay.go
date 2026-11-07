package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/dmitriyb/faber/security"
)

// maxAgentMessage bounds one agent-protocol message the relay forwards in
// either direction (OpenSSH's own AGENT_MAX_LEN).
const maxAgentMessage = 256 << 10

// Relay is the box's end of the identity binding: the host execs it into
// the container the moment the container runs, with the host's agent server
// on the other side of its stdin and stdout. It listens on SSH_AUTH_SOCK,
// gives the socket to the run user and drops to that user, then forwards
// one agent message at a time — a client's request out on stdout, the
// host's reply in on stdin — so the single stream stays request–reply
// aligned however many clients connect. A client that sends a malformed
// length is disconnected alone; the host's stream ending, or ctx, ends the
// relay (a cancellation that lands while a reply is awaited waits for that
// reply or the stream's end: the one reader is blocked on the host, who in
// practice is gone together with the container). Nothing but length-checked
// bytes crosses it.
func Relay(ctx context.Context, env *BoxEnv, in io.Reader, out io.Writer, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sock := env.AgentSocket
	if sock == "" {
		return fmt.Errorf("relay: %s is not set on this box", security.EnvSSHAuthSock)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", sock, err)
	}
	defer ln.Close()
	if err := os.Chmod(sock, 0o600); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	if os.Getuid() == 0 && env.RunUID != 0 {
		// The socket first, while the directory is still root's: once the
		// run user owns the directory it could swap the socket for a link
		// and have root chown whatever the link names.
		for _, p := range []string{sock, filepath.Dir(sock)} {
			if err := os.Chown(p, env.RunUID, env.RunGID); err != nil {
				return fmt.Errorf("relay: chown %s: %w", p, err)
			}
		}
		if err := dropTo(env.RunUID, env.RunGID); err != nil {
			return fmt.Errorf("relay: %w", err)
		}
	}
	log.InfoContext(ctx, "agent relay listening", "socket", sock)

	r := &relay{out: out, replies: make(chan []byte)}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancel = cancel
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	// One reader owns the host's stream, so its end is noticed at once —
	// not only at the next exchange — and the relay ends with it.
	go func() {
		defer close(r.replies)
		for {
			msg, err := readAgentMessage(in)
			if err != nil {
				r.fail(fmt.Errorf("from host: %w", err))
				return
			}
			select {
			case r.replies <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			// The listener closed: by the host's stream ending, or by ctx.
			// A client mid-conversation is cut off: its agent is gone.
			r.closeConns()
			wg.Wait()
			if herr := r.hostErr(); herr != nil && !errors.Is(herr, io.EOF) {
				return fmt.Errorf("relay: %w", herr)
			}
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.serve(conn)
		}()
	}
}

// relay pumps messages between the box's clients and the host's stream.
type relay struct {
	out     io.Writer
	replies chan []byte // the host's messages, in order, from the one reader
	cancel  context.CancelFunc

	mu    sync.Mutex // one exchange at a time: the stream is request–reply
	errMu sync.Mutex
	err   error // the host's stream failing, which ends the relay

	connMu  sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool // closeConns ran: a connection accepted since is closed on arrival
}

// track registers a live connection; closeConns ends them all on shutdown,
// and one that slips in between the listener closing and its registration
// is closed here.
func (r *relay) track(conn net.Conn, live bool) {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if r.conns == nil {
		r.conns = map[net.Conn]struct{}{}
	}
	if live {
		if r.closing {
			conn.Close()
		}
		r.conns[conn] = struct{}{}
	} else {
		delete(r.conns, conn)
	}
}

func (r *relay) closeConns() {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	r.closing = true
	for conn := range r.conns {
		conn.Close()
	}
}

// serve forwards one connection's requests until it closes or misbehaves.
func (r *relay) serve(conn net.Conn) {
	r.track(conn, true)
	defer r.track(conn, false)
	defer conn.Close()
	for {
		msg, err := readAgentMessage(conn)
		if err != nil {
			return
		}
		reply, err := r.exchange(msg)
		if err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

// exchange sends one request to the host and waits for its one reply. A
// failure of the host's stream is recorded once and ends the relay.
func (r *relay) exchange(msg []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.hostErr(); err != nil {
		return nil, err
	}
	if _, err := r.out.Write(msg); err != nil {
		return nil, r.fail(fmt.Errorf("to host: %w", err))
	}
	reply, ok := <-r.replies
	if !ok {
		if err := r.hostErr(); err != nil {
			return nil, err
		}
		return nil, errors.New("relay ending")
	}
	return reply, nil
}

// fail records the host's stream failing, once, and ends the relay.
func (r *relay) fail(err error) error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.err == nil {
		r.err = err
		r.cancel()
	}
	return r.err
}

// hostErr is the recorded failure; the host closing its side (EOF) is an
// orderly end and reads as none to the caller of Relay.
func (r *relay) hostErr() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

// readAgentMessage reads one length-prefixed agent message and returns it
// prefix included, refusing a length outside 1..maxAgentMessage.
func readAgentMessage(rd io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(rd, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n <= 0 || n > maxAgentMessage {
		return nil, fmt.Errorf("agent message length %d outside 1..%d", n, maxAgentMessage)
	}
	msg := make([]byte, 4+n)
	copy(msg, hdr[:])
	if _, err := io.ReadFull(rd, msg[4:]); err != nil {
		return nil, err
	}
	return msg, nil
}
