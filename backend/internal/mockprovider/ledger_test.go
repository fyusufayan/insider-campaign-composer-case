package mockprovider

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// durable is one provider "process": a server on a ledger path, served over
// TCP, with the production client.
type durable struct {
	t      *testing.T
	path   string
	srv    *Server
	hs     *httptest.Server
	client *provider.Client
}

func startDurable(t *testing.T, path string, cfg Config) *durable {
	t.Helper()
	cfg.LedgerPath = path
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	d := &durable{t: t, path: path, srv: srv, hs: httptest.NewServer(srv.Handler())}
	d.client, err = provider.NewClient(provider.ClientConfig{BaseURL: d.hs.URL, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stop)
	return d
}

// stop is a process exit: no further requests, the journal is released.
func (d *durable) stop() {
	if d.hs == nil {
		return
	}
	d.hs.Close()
	_ = d.srv.Close()
	d.hs = nil
}

func (d *durable) restart() *durable {
	d.t.Helper()
	d.stop()
	return startDurable(d.t, d.path, Config{})
}

func (d *durable) send(m provider.Message) provider.Result {
	return d.client.Send(context.Background(), m)
}

func records(t *testing.T, path string) []ledgerRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recs, end, err := parseLedger(data)
	if err != nil || end != int64(len(data)) {
		t.Fatalf("ledger: %v (end %d of %d)", err, end, len(data))
	}
	return recs
}

var (
	msgA = provider.Message{Channel: provider.ChannelSMS, IdempotencyKey: "c1:1:sms", To: "+905550001234", Text: "Hi Ayşe, 30% off today."}
	msgB = provider.Message{Channel: provider.ChannelEmail, IdempotencyKey: "c1:1:email", To: "ayse@example.test",
		Subject: "Your cart", Text: "Hello Ayşe,\n\nYour cart is waiting.\n\n-- \nfooter"}
)

func ledgerPath(t *testing.T) string { return filepath.Join(t.TempDir(), "ledger.journal") }

// 1 + 2 + 10: the first request creates exactly one committed record (key,
// channel, fingerprint, message id — no destination, no text); a replay
// without restart returns the same id and writes nothing.
func TestDurableFirstAcceptanceAndReplay(t *testing.T) {
	d := startDurable(t, ledgerPath(t), Config{})
	first := d.send(msgA)
	if first.Outcome != provider.OutcomeAccepted {
		t.Fatalf("first = %+v", first)
	}
	recs := records(t, d.path)
	if len(recs) != 1 || recs[0].Seq != 1 || recs[0].Key != msgA.IdempotencyKey || recs[0].Channel != provider.ChannelSMS ||
		recs[0].MessageID != first.MessageID || len(recs[0].Hash) != 64 {
		t.Fatalf("records = %+v", recs)
	}
	for i := 0; i < 3; i++ {
		if r := d.send(msgA); r.Outcome != provider.OutcomeReplayed || r.MessageID != first.MessageID {
			t.Fatalf("replay = %+v", r)
		}
	}
	d.send(msgB)
	if n := len(records(t, d.path)); n != 2 {
		t.Errorf("records after replays = %d", n)
	}
	data, _ := os.ReadFile(d.path)
	for _, secret := range []string{msgA.To, msgB.To, "Ayşe", "30% off", "Your cart", "footer", "example.test", "+90"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("ledger file contains %q", secret)
		}
	}
	if st := d.srv.Stats(); !st.Durable {
		t.Error("stats do not report durability")
	}
}

// 3 + 4: after a process restart on the same ledger, the same key with the
// same payload replays the original id (no second acceptance); with a
// changed payload it is rejected (422); new message ids never collide.
func TestDurableReplayAfterRestart(t *testing.T) {
	d := startDurable(t, ledgerPath(t), Config{})
	a, b := d.send(msgA), d.send(msgB)
	d = d.restart()
	if r := d.send(msgA); r.Outcome != provider.OutcomeReplayed || r.MessageID != a.MessageID {
		t.Fatalf("sms after restart = %+v", r)
	}
	if r := d.send(msgB); r.Outcome != provider.OutcomeReplayed || r.MessageID != b.MessageID {
		t.Fatalf("email after restart = %+v", r)
	}
	for _, m := range []provider.Message{
		func() provider.Message { m := msgA; m.Text += "!"; return m }(),
		func() provider.Message { m := msgA; m.To = "+905559999999"; return m }(),
		func() provider.Message { m := msgB; m.Subject = "Other"; return m }(),
	} {
		if r := d.send(m); r.Outcome != provider.OutcomePermanentError || r.HTTPStatus != 422 || r.ErrorCode != provider.CodeIdempotencyKeyReuse {
			t.Errorf("changed payload after restart = %+v", r)
		}
	}
	st := d.srv.Stats()
	if c := st.Channels[provider.ChannelSMS]; c.Accepted != 0 || c.Replayed != 1 || c.Messages != 1 {
		t.Errorf("sms stats after restart = %+v", c)
	}
	c := msgA
	c.IdempotencyKey = "c1:2:sms"
	r := d.send(c)
	if r.Outcome != provider.OutcomeAccepted || r.MessageID == a.MessageID || r.MessageID == b.MessageID {
		t.Errorf("new key after restart = %+v", r)
	}
	if recs := records(t, d.path); len(recs) != 3 || recs[2].Seq != 3 {
		t.Errorf("records = %+v", recs)
	}
	// A second process on the same ledger is refused (exclusive lock).
	if _, err := New(Config{LedgerPath: d.path}, slog.New(slog.NewTextHandler(io.Discard, nil))); !errors.Is(err, ErrLedgerUnavailable) {
		t.Errorf("second process: %v", err)
	}
}

// 5: concurrent identical requests produce one durable acceptance.
func TestDurableConcurrentIdenticalRequests(t *testing.T) {
	d := startDurable(t, ledgerPath(t), Config{})
	const n = 32
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got = map[provider.Outcome]int{}
		ids = map[string]bool{}
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := d.send(msgA)
			mu.Lock()
			got[r.Outcome]++
			ids[r.MessageID] = true
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if got[provider.OutcomeAccepted] != 1 || got[provider.OutcomeReplayed] != n-1 || len(ids) != 1 {
		t.Errorf("outcomes = %v, ids = %v", got, ids)
	}
	if recs := records(t, d.path); len(recs) != 1 {
		t.Errorf("records = %d", len(recs))
	}
}

// 6 (provider side): a committed acceptance whose response is lost (the
// injected timeout commits, then stalls), then a restart: the retry is a
// replay of the original id.
func TestDurableLostResponseThenRestart(t *testing.T) {
	path := ledgerPath(t)
	cfg := Config{TimeoutStall: 10 * time.Second}
	srv, err := New(Config{LedgerPath: path, TimeoutStall: cfg.TimeoutStall}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithFaults(func(Request) Fault { return FaultTimeout }))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	client, _ := provider.NewClient(provider.ClientConfig{BaseURL: hs.URL, RequestTimeout: 200 * time.Millisecond})
	if r := client.Send(context.Background(), msgA); r.Outcome != provider.OutcomeTimeout {
		t.Fatalf("first = %+v", r)
	}
	recs := records(t, path)
	if len(recs) != 1 {
		t.Fatalf("committed records = %d", len(recs))
	}
	hs.Close()
	_ = srv.Close()
	d := startDurable(t, path, Config{})
	if r := d.send(msgA); r.Outcome != provider.OutcomeReplayed || r.MessageID != recs[0].MessageID {
		t.Fatalf("retry after restart = %+v", r)
	}
	if n := len(records(t, path)); n != 1 {
		t.Errorf("records = %d", n)
	}
}

// 7: a persistence failure accepts nothing (503, transient for the client),
// keeps failing closed until restart, and the restart shows no acceptance.
func TestDurablePersistenceFailureAcceptsNothing(t *testing.T) {
	d := startDurable(t, ledgerPath(t), Config{})
	d.send(msgB) // one committed record before the failure
	d.srv.mu.Lock()
	d.srv.journal.failNext = func() error { return errors.New("injected disk failure") }
	d.srv.mu.Unlock()
	for i := 0; i < 2; i++ {
		r := d.send(msgA)
		if r.Outcome != provider.OutcomeTransientError || r.HTTPStatus != 503 || r.ErrorCode != "storage_unavailable" || r.MessageID != "" {
			t.Fatalf("attempt %d = %+v", i, r)
		}
	}
	d.srv.mu.Lock()
	d.srv.journal.failNext = nil
	d.srv.mu.Unlock()
	if r := d.send(msgA); r.HTTPStatus != 503 { // broken until restart, even though the hook is gone
		t.Errorf("after the failure = %+v", r)
	}
	if st := d.srv.Stats().Channels[provider.ChannelSMS]; st.StorageFailures != 3 || st.Accepted != 0 || st.Messages != 0 {
		t.Errorf("stats = %+v", st)
	}
	if recs := records(t, d.path); len(recs) != 1 || recs[0].Key != msgB.IdempotencyKey {
		t.Fatalf("records = %+v", recs)
	}
	d = d.restart()
	if r := d.send(msgA); r.Outcome != provider.OutcomeAccepted {
		t.Errorf("after restart = %+v", r)
	}
	// A real write error (the file descriptor is gone) also fails closed.
	d.srv.mu.Lock()
	_ = d.srv.journal.f.Close()
	d.srv.mu.Unlock()
	m := msgA
	m.IdempotencyKey = "c1:9:sms"
	if r := d.send(m); r.HTTPStatus != 503 || r.ErrorCode != "storage_unavailable" {
		t.Errorf("closed file = %+v", r)
	}
}

func appendRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// 8: an interrupted append (torn tail: a partial frame header, or a frame
// that extends past the end of the file) was never acknowledged; recovery
// truncates exactly that tail, keeps every committed record, and the key of
// the torn record is a NEW acceptance. A complete, valid frame whose fsync
// outcome was unknown (the client got 503) counts as committed: its retry
// is a replay, never a second acceptance.
func TestDurableInterruptedCommitRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail func(frame []byte) []byte
	}{
		{"partial frame header", func(f []byte) []byte { return f[:5] }},
		{"payload cut short", func(f []byte) []byte { return f[:len(f)/2] }},
		{"checksum cut short", func(f []byte) []byte { return f[:len(f)-2] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startDurable(t, ledgerPath(t), Config{})
			a := d.send(msgA)
			d.stop()
			before, _ := os.ReadFile(d.path)
			frame, err := encodeFrame(ledgerRecord{Seq: 2, Key: msgB.IdempotencyKey, Channel: provider.ChannelEmail,
				Hash: strings.Repeat("ab", 32), MessageID: "mp_email_0000000002"})
			if err != nil {
				t.Fatal(err)
			}
			torn := tc.tail(frame)
			appendRaw(t, d.path, torn)

			_, _, rec, err := openLedgerForTest(d.path)
			if err != nil || rec.TornTailBytes != int64(len(torn)) || rec.Records != 1 {
				t.Fatalf("recovery = %+v %v", rec, err)
			}
			if after, _ := os.ReadFile(d.path); !bytes.Equal(after, before) {
				t.Fatal("recovery did not truncate exactly the torn tail")
			}
			d = startDurable(t, d.path, Config{})
			if r := d.send(msgA); r.Outcome != provider.OutcomeReplayed || r.MessageID != a.MessageID {
				t.Errorf("committed record lost: %+v", r)
			}
			if r := d.send(msgB); r.Outcome != provider.OutcomeAccepted {
				t.Errorf("torn (unacknowledged) record = %+v", r)
			}
		})
	}
	t.Run("complete frame with unknown fsync outcome", func(t *testing.T) {
		d := startDurable(t, ledgerPath(t), Config{})
		d.send(msgA)
		d.stop()
		hash := payloadHash(provider.ChannelEmail, parsed{to: msgB.To, subject: msgB.Subject, text: msgB.Text})
		frame, _ := encodeFrame(ledgerRecord{Seq: 2, Key: msgB.IdempotencyKey, Channel: provider.ChannelEmail,
			Hash: hashHex(hash), MessageID: "mp_email_0000000002"})
		appendRaw(t, d.path, frame)
		d = startDurable(t, d.path, Config{})
		if r := d.send(msgB); r.Outcome != provider.OutcomeReplayed || r.MessageID != "mp_email_0000000002" {
			t.Errorf("retry = %+v", r)
		}
	})
}

func openLedgerForTest(path string) (*ledger, []ledgerRecord, Recovery, error) {
	l, recs, rec, err := openLedger(path)
	if l != nil {
		l.close()
	}
	return l, recs, rec, err
}

// 9: a ledger whose integrity cannot be established stops the server from
// starting (fail closed) — it never starts with a fresh, empty ledger — and
// the file is left untouched for inspection.
func TestDurableCorruptLedgerFailsClosed(t *testing.T) {
	good := func(t *testing.T) (string, []byte) {
		d := startDurable(t, ledgerPath(t), Config{})
		d.send(msgA)
		d.send(msgB)
		d.stop()
		data, _ := os.ReadFile(d.path)
		return d.path, data
	}
	frame := func(r ledgerRecord) []byte { f, _ := encodeFrame(r); return f }
	for _, tc := range []struct {
		name   string
		mutate func(data []byte) []byte
	}{
		{"flipped payload byte", func(d []byte) []byte { d = bytes.Clone(d); d[headerSize+12] ^= 0x01; return d }},
		{"flipped checksum byte", func(d []byte) []byte { d = bytes.Clone(d); d[len(d)-1] ^= 0x01; return d }},
		{"garbage after the last frame", func(d []byte) []byte { return append(bytes.Clone(d), []byte("garbage!garbage!")...) }},
		{"zero-filled tail", func(d []byte) []byte { return append(bytes.Clone(d), make([]byte, 64)...) }},
		{"bad header", func(d []byte) []byte { d = bytes.Clone(d); d[0] = 'X'; return d }},
		{"unsupported version", func(d []byte) []byte { d = bytes.Clone(d); d[11] = 9; return d }},
		{"truncated header", func(d []byte) []byte { return d[:6] }},
		{"sequence gap", func(d []byte) []byte {
			return append(bytes.Clone(d), frame(ledgerRecord{Seq: 5, Key: "k5", Channel: provider.ChannelSMS, Hash: strings.Repeat("0", 64), MessageID: "m5"})...)
		}},
		{"duplicate key", func(d []byte) []byte {
			return append(bytes.Clone(d), frame(ledgerRecord{Seq: 3, Key: msgA.IdempotencyKey, Channel: provider.ChannelSMS, Hash: strings.Repeat("0", 64), MessageID: "m3"})...)
		}},
		{"unknown field", func(d []byte) []byte {
			payload := []byte(`{"seq":3,"key":"k3","channel":"sms","payload_sha256":"` + strings.Repeat("0", 64) + `","message_id":"m3","to":"+905550001234"}`)
			f := append([]byte(frameMagic), 0, 0, 0, byte(len(payload)))
			f = append(f, payload...)
			crc := make([]byte, 4)
			c := crcOf(payload)
			crc[0], crc[1], crc[2], crc[3] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
			return append(append(bytes.Clone(d), f...), crc...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, data := good(t)
			bad := tc.mutate(data)
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := New(Config{LedgerPath: path}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if !errors.Is(err, ErrLedgerCorrupt) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), msgA.IdempotencyKey) {
				t.Errorf("error echoes ledger content: %v", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, bad) {
				t.Error("a corrupt ledger was modified")
			}
		})
	}
}

// The destructive reset is opt-in; when enabled it removes every key, and
// a later restart sees an empty ledger.
func TestDurableResetIsExplicit(t *testing.T) {
	d := startDurable(t, ledgerPath(t), Config{})
	a := d.send(msgA)
	resp, err := http.Post(d.hs.URL+"/admin/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(records(t, d.path)) != 1 {
		t.Fatalf("reset without opt-in: %d", resp.StatusCode)
	}
	d.stop()
	d = startDurable(t, d.path, Config{AllowReset: true})
	resp, _ = http.Post(d.hs.URL+"/admin/reset", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(records(t, d.path)) != 0 {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	d = d.restart()
	if r := d.send(msgA); r.Outcome != provider.OutcomeAccepted || r.MessageID != a.MessageID {
		// After a reset ids restart at 1: the guarantee for earlier keys is gone.
		t.Errorf("after reset = %+v (a %s)", r, a.MessageID)
	}
}

// Per-campaign counts: ledger entries by key prefix.
func TestStatsByKeyPrefix(t *testing.T) {
	d := startDurable(t, ledgerPath(t), Config{})
	d.send(msgA)
	d.send(msgB)
	other := msgA
	other.IdempotencyKey = "c2:1:sms"
	d.send(other)
	d = d.restart()
	st := d.srv.StatsFor("c1:")
	if st.PrefixMessages[provider.ChannelSMS] != 1 || st.PrefixMessages[provider.ChannelEmail] != 1 ||
		st.Channels[provider.ChannelSMS].Messages != 2 {
		t.Errorf("stats = %+v", st)
	}
	resp, _ := http.Get(d.hs.URL + "/admin/stats?key_prefix=" + "c1%27%3B")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid prefix: %d", resp.StatusCode)
	}
}

func crcOf(b []byte) uint32 { return crc32.Checksum(b, crcTable) }
