package mockprovider

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Durable idempotency ledger: a framed, append-only journal in one file.
//
// File layout:
//
//	header  = "MPLEDGER" (8 bytes) + format version 1 (uint32, big endian)
//	frame   = "MPR1" (4) + payload length n (uint32 BE) + payload (n bytes, JSON) + CRC-32C of payload (uint32 BE)
//	payload = {"seq", "key", "channel", "payload_sha256", "message_id"}
//
// A record holds the idempotency key, the channel, the SHA-256 fingerprint of
// the canonical payload (channel, destination, subject, text) and the
// original message id — never the destination or the message text.
//
// Commit point: a message is logically accepted iff its frame was written
// completely and fsync of the file returned success. Only then is the key
// added to the in-memory index and is 202 returned. Appends are serialized
// by the server mutex and written at the end of the last committed frame.
//
// Recovery on open (the index is rebuilt from committed frames only):
//   - every complete frame must have the magic, a bounded length, a matching
//     CRC, a well-formed payload, seq = previous seq + 1 and a key not seen
//     before; anything else is corruption → open fails (fail closed: the
//     server does not start with a fresh or partial ledger);
//   - a torn tail — fewer than 8 bytes after the last complete frame, or a
//     frame with the correct magic whose declared length extends past the
//     end of the file — can only be an append whose fsync never returned
//     (never acknowledged), because appends are serialized and acknowledged
//     only after fsync. It is truncated (and the truncation fsynced), and
//     the number of discarded bytes is reported;
//   - 8 or more trailing bytes without the frame magic are corruption, and
//     so is a file shorter than the header (it cannot be told apart from a
//     truncated ledger).
//
// After a failed write or fsync the ledger is marked broken: every further
// acceptance fails (503) until the process restarts and recovery decides —
// from what actually reached the file — whether that record committed. A
// request answered 503 may therefore turn out committed after a restart;
// its retry with the same key is then a replay, never a second acceptance.
//
// Limits: this relies on the local filesystem honouring fsync and on
// exclusive access (an flock is taken). It does not protect against media
// corruption beyond what the CRC detects, lying disks or network
// filesystems without POSIX locking.

var (
	// ErrLedgerCorrupt: the ledger file failed validation; the provider
	// refuses to start rather than forget accepted keys.
	ErrLedgerCorrupt = errors.New("mockprovider: idempotency ledger is corrupt")
	// ErrLedgerUnavailable: the ledger could not be written; acceptances fail
	// until the process restarts.
	ErrLedgerUnavailable = errors.New("mockprovider: idempotency ledger unavailable")
)

const (
	ledgerMagic    = "MPLEDGER"
	ledgerVersion  = 1
	headerSize     = 12
	frameMagic     = "MPR1"
	frameOverhead  = 12 // magic + length + crc
	maxPayloadSize = 4 << 10
)

var (
	crcTable    = crc32.MakeTable(crc32.Castagnoli)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ledgerRecord is one committed acceptance.
type ledgerRecord struct {
	Seq       int64            `json:"seq"`
	Key       string           `json:"key"`
	Channel   provider.Channel `json:"channel"`
	Hash      string           `json:"payload_sha256"`
	MessageID string           `json:"message_id"`
}

// Recovery describes what opening the ledger found.
type Recovery struct {
	Records int
	// TornTailBytes is the size of an unacknowledged partial append that
	// was truncated (0 if none).
	TornTailBytes int64
	Created       bool
}

type ledger struct {
	f      *os.File
	size   int64 // end of the last committed frame
	broken error
	// failNext is a test hook: an error returned instead of writing.
	failNext func() error
}

func encodeFrame(rec ledgerRecord) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPayloadSize {
		return nil, errors.New("record too large")
	}
	frame := make([]byte, 0, frameOverhead+len(payload))
	frame = append(frame, frameMagic...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	return binary.BigEndian.AppendUint32(frame, crc32.Checksum(payload, crcTable)), nil
}

func header() []byte {
	return binary.BigEndian.AppendUint32([]byte(ledgerMagic), ledgerVersion)
}

// openLedger opens (or creates) the ledger at path, takes an exclusive lock
// and returns the committed records.
func openLedger(path string) (*ledger, []ledgerRecord, Recovery, error) {
	var rec Recovery
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, rec, fmt.Errorf("%w: open: %v", ErrLedgerUnavailable, sanitizePathErr(err))
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, nil, rec, fmt.Errorf("%w: the ledger is locked by another process", ErrLedgerUnavailable)
	}
	fail := func(e error) (*ledger, []ledgerRecord, Recovery, error) {
		f.Close() // releases the lock
		return nil, nil, rec, e
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fail(fmt.Errorf("%w: read: %v", ErrLedgerUnavailable, sanitizePathErr(err)))
	}
	if len(data) == 0 {
		if _, err := f.WriteAt(header(), 0); err != nil {
			return fail(fmt.Errorf("%w: write header: %v", ErrLedgerUnavailable, sanitizePathErr(err)))
		}
		if err := f.Sync(); err != nil {
			return fail(fmt.Errorf("%w: sync: %v", ErrLedgerUnavailable, sanitizePathErr(err)))
		}
		if err := syncDir(filepath.Dir(path)); err != nil {
			return fail(fmt.Errorf("%w: sync directory: %v", ErrLedgerUnavailable, sanitizePathErr(err)))
		}
		rec.Created = true
		return &ledger{f: f, size: headerSize}, nil, rec, nil
	}
	records, committed, err := parseLedger(data)
	if err != nil {
		return fail(err)
	}
	if torn := int64(len(data)) - committed; torn > 0 {
		if err := f.Truncate(committed); err != nil {
			return fail(fmt.Errorf("%w: truncate torn tail: %v", ErrLedgerUnavailable, sanitizePathErr(err)))
		}
		if err := f.Sync(); err != nil {
			return fail(fmt.Errorf("%w: sync: %v", ErrLedgerUnavailable, sanitizePathErr(err)))
		}
		rec.TornTailBytes = torn
	}
	rec.Records = len(records)
	return &ledger{f: f, size: committed}, records, rec, nil
}

// parseLedger validates data and returns the committed records and the
// offset of the end of the last committed frame (everything after it is a
// torn tail). See the recovery rules above.
func parseLedger(data []byte) ([]ledgerRecord, int64, error) {
	corrupt := func(off int, why string) error {
		return fmt.Errorf("%w: %s at offset %d", ErrLedgerCorrupt, why, off)
	}
	if len(data) < headerSize {
		// Even an interrupted creation is not repaired automatically: a
		// short file cannot be told apart from a truncated ledger whose
		// records were lost, so the operator must decide (fail closed).
		return nil, 0, corrupt(0, "incomplete header")
	}
	if string(data[:8]) != ledgerMagic {
		return nil, 0, corrupt(0, "not a ledger file")
	}
	if v := binary.BigEndian.Uint32(data[8:12]); v != ledgerVersion {
		return nil, 0, corrupt(8, fmt.Sprintf("unsupported version %d", v))
	}
	var (
		records []ledgerRecord
		keys    = map[string]bool{}
		off     = headerSize
	)
	for off < len(data) {
		rest := data[off:]
		if len(rest) < 8 {
			return records, int64(off), nil // torn tail: frame header incomplete
		}
		if string(rest[:4]) != frameMagic {
			return nil, 0, corrupt(off, "bad frame magic")
		}
		n := int(binary.BigEndian.Uint32(rest[4:8]))
		if n == 0 || n > maxPayloadSize {
			return nil, 0, corrupt(off, "bad frame length")
		}
		if len(rest) < frameOverhead+n {
			return records, int64(off), nil // torn tail: frame extends past the end
		}
		payload := rest[8 : 8+n]
		if crc32.Checksum(payload, crcTable) != binary.BigEndian.Uint32(rest[8+n:12+n]) {
			return nil, 0, corrupt(off, "checksum mismatch")
		}
		var r ledgerRecord
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, 0, corrupt(off, "malformed record")
		}
		switch {
		case r.Seq != int64(len(records))+1:
			return nil, 0, corrupt(off, "sequence gap")
		case !validKey(r.Key) || keys[r.Key]:
			return nil, 0, corrupt(off, "invalid or duplicate key")
		case r.Channel != provider.ChannelSMS && r.Channel != provider.ChannelEmail:
			return nil, 0, corrupt(off, "invalid channel")
		case !hashPattern.MatchString(r.Hash) || r.MessageID == "" || len(r.MessageID) > 64:
			return nil, 0, corrupt(off, "invalid record fields")
		}
		keys[r.Key] = true
		records = append(records, r)
		off += frameOverhead + n
	}
	return records, int64(off), nil
}

// append commits rec: write the frame after the last committed one, then
// fsync. Any failure marks the ledger broken (no further acceptances).
func (l *ledger) append(rec ledgerRecord) error {
	if l.broken != nil {
		return l.broken
	}
	frame, err := encodeFrame(rec)
	if err != nil {
		return fmt.Errorf("%w: encode: %v", ErrLedgerUnavailable, err)
	}
	if l.failNext != nil {
		if err := l.failNext(); err != nil {
			l.broken = fmt.Errorf("%w: %v", ErrLedgerUnavailable, err)
			return l.broken
		}
	}
	if _, err := l.f.WriteAt(frame, l.size); err != nil {
		l.broken = fmt.Errorf("%w: write: %v", ErrLedgerUnavailable, sanitizePathErr(err))
		return l.broken
	}
	if err := l.f.Sync(); err != nil {
		l.broken = fmt.Errorf("%w: sync: %v", ErrLedgerUnavailable, sanitizePathErr(err))
		return l.broken
	}
	l.size += int64(len(frame))
	return nil
}

// reset destroys every record (explicit, destructive test operation).
func (l *ledger) reset() error {
	if err := l.f.Truncate(headerSize); err != nil {
		return fmt.Errorf("%w: truncate: %v", ErrLedgerUnavailable, sanitizePathErr(err))
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("%w: sync: %v", ErrLedgerUnavailable, sanitizePathErr(err))
	}
	l.size, l.broken = headerSize, nil
	return nil
}

func (l *ledger) close() error { return l.f.Close() }

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// sanitizePathErr keeps the operation and cause of a filesystem error
// without repeating paths in API-facing contexts.
func sanitizePathErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %v", pe.Op, pe.Err)
	}
	return err
}

func hashHex(h [32]byte) string { return hex.EncodeToString(h[:]) }
