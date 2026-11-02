package security

import (
	"context"
	"crypto/rand"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// proxyClient dials a contributed socket as a box's ssh would.
func proxyClient(t *testing.T, sock string) (agent.ExtendedAgent, func()) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	return agent.NewClient(conn), func() { conn.Close() }
}

// Verifies 1b0f6b9009af: through the box's socket, List exposes the role's
// one key and Sign produces a signature that verifies against it — the
// proxy speaks the agent protocol a real ssh client uses.
func TestSignQueue_ProxyListsAndSigns(t *testing.T) {
	fa := newFakeAgent()
	b := identityBinding(fa, nil)
	c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer b.Close(context.Background())
	defer c.Teardown(context.Background())
	client, done := proxyClient(t, contributedSocket(t, c.Args))
	defer done()
	keys, err := client.List()
	if err != nil || len(keys) != 1 || keys[0].Comment != "implementer" {
		t.Fatalf("List = %v, %v; want the implementer key", keys, err)
	}
	data := []byte("challenge")
	sig, err := client.Sign(keys[0], data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := keys[0].Verify(data, sig); err != nil {
		t.Fatalf("signature does not verify against the exposed key: %v", err)
	}
}

// Verifies 1b0f6b9009af: concurrent signatures from several boxes of the
// same role pass the queue one at a time — the upstream never sees two
// Sign calls overlap, which is the whole hardware-token fix.
func TestSignQueue_SerializesConcurrentSigns(t *testing.T) {
	fa := newFakeAgent()
	fa.signDelay = 5 * time.Millisecond
	b := identityBinding(fa, nil)
	defer b.Close(context.Background())
	const boxes = 8
	var contribs []Contribution
	for i := 0; i < boxes; i++ {
		c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
		if err != nil {
			t.Fatalf("Prepare %d: %v", i, err)
		}
		contribs = append(contribs, c)
	}
	defer func() {
		for _, c := range contribs {
			c.Teardown(context.Background())
		}
	}()
	var wg sync.WaitGroup
	for _, c := range contribs {
		wg.Add(1)
		go func(sock string) {
			defer wg.Done()
			client, done := proxyClient(t, sock)
			defer done()
			keys, err := client.List()
			if err != nil || len(keys) != 1 {
				t.Errorf("List: %v %v", keys, err)
				return
			}
			if _, err := client.Sign(keys[0], []byte("x")); err != nil {
				t.Errorf("Sign: %v", err)
			}
		}(contributedSocket(t, c.Args))
	}
	wg.Wait()
	if fa.maxInSign != 1 {
		t.Fatalf("upstream saw %d concurrent signs, want 1 (the queue)", fa.maxInSign)
	}
	if len(fa.starts) != 1 {
		t.Fatalf("want one role agent for %d boxes, got %d spawns", boxes, len(fa.starts))
	}
}

// Verifies 1b0f6b9009af: a box cannot grow, shrink, or lock its agent, and
// cannot sign with a key that is not its identity.
func TestSignQueue_ProxyIsReadOnly(t *testing.T) {
	fa := newFakeAgent()
	b := identityBinding(fa, nil)
	c, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer b.Close(context.Background())
	defer c.Teardown(context.Background())
	client, done := proxyClient(t, contributedSocket(t, c.Args))
	defer done()
	_, priv, err := ed25519GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Add(agent.AddedKey{PrivateKey: priv, Comment: "planted"}); err == nil {
		t.Error("Add must be refused")
	}
	if err := client.RemoveAll(); err == nil {
		t.Error("RemoveAll must be refused")
	}
	if err := client.Lock([]byte("pw")); err == nil {
		t.Error("Lock must be refused")
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Sign(signer.PublicKey(), []byte("x")); err == nil {
		t.Errorf("Sign with a foreign key: err = %v, want refusal", err)
	}
	keys, err := client.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("after refusals List = %v, %v; want the one role key", keys, err)
	}
}

// Verifies 1b0f6b9009af: two roles get two run-scoped agents, attempts of
// one role reuse theirs, and Close stops both and removes their dirs.
func TestSignQueue_RoleAgentsReusedAndClosed(t *testing.T) {
	fa := newFakeAgent()
	b := identityBinding(fa, nil)
	c1, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := b.Prepare(context.Background(), implementStep(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c3, err := b.Prepare(context.Background(), mergeStep(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Contribution{c1, c2, c3} {
		if err := c.Teardown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(fa.starts) != 2 {
		t.Fatalf("want two role agents (implementer, merger), got spawns %q", fa.starts)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := fa.liveAgents(); len(live) != 0 {
		t.Fatalf("live after Close: %q", live)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

func ed25519GenerateKey() (pub, priv interface{}, err error) {
	return ed25519Gen(rand.Reader)
}
