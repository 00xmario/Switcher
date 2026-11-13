package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DeviceKey is the key a computer's ledger is kept under: a hash of the id
// it sends its usage with (internal/remote's UsageID, made from a secret of
// its own for this host). It stays the same when the computer pairs again,
// so its usage is never counted twice, and the id itself is not passed on
// to other computers.
func DeviceKey(id string) string {
	sum := sha256.Sum256([]byte("switcher-usage|" + id))
	return hex.EncodeToString(sum[:16])
}

// ErrStoreFull refuses a computer's ledger when the host keeps as many as
// it can, all of computers still paired.
var ErrStoreFull = errors.New("this host keeps the usage of as many computers as it can")

// maxStoredDevices bounds how many computers' ledgers a host keeps.
const maxStoredDevices = 32

// storedLedgerLimit bounds a kept ledger's file: what a host reads back. It
// is past the largest upload by the room the file's own fields take.
const storedLedgerLimit = 17 << 20

// keptLedgerLimit is the largest ledger Put keeps; tests lower it.
var keptLedgerLimit = storedLedgerLimit

// writeEvery is how often a computer's changed ledger is written to disk at
// most; in between the latest is kept in memory and written when the time
// comes. unchangedSave is the same for one whose usage did not change, so
// the time it was last sent survives a restart.
const (
	writeEvery    = 5 * time.Minute
	unchangedSave = 30 * time.Minute
)

// DeviceStore keeps, on a host, the ledgers its paired computers send: one
// per computer, on disk, so a computer that is away still counts. Each
// pairing has one: a pairing that sends another computer's ledger gives up
// the one it sent before. A computer's id is made from its own secret (the
// host passes on only its hash), so the pairing that sends it last holds its
// ledger: a computer that paired again, after an unpairing that never
// reached the host, keeps its numbers. A ledger counts while its pairing is
// in place; the owner removing the pairing removes it.
type DeviceStore struct {
	dir     string
	mu      sync.Mutex
	devices map[string]*storedLedger
	version uint64                 // raised by every change, for what is made from the ledgers
	timers  map[string]*time.Timer // pending writes of changed ledgers
	closing bool                   // flushed for good: every ledger is written at once
}

type storedLedger struct {
	Key       string  `json:"key"`
	Device    string  `json:"device"` // the pairing that sent it last
	Name      string  `json:"name"`
	UpdatedMs int64   `json:"updated_ms"`
	Ledger    *Ledger `json:"ledger"`

	sum     [32]byte // of the ledger's usage, to tell an unchanged one
	savedMs int64    // when the file was last written
	dirty   bool     // changed since it was written
	failed  bool     // its last write failed
}

// ErrLedgerTooLarge refuses a ledger larger than a host keeps.
var ErrLedgerTooLarge = errors.New("this computer's usage is larger than a host keeps")

// OpenDeviceStore reads the ledgers kept in dir.
func OpenDeviceStore(dir string) *DeviceStore {
	s := &DeviceStore{dir: dir, devices: map[string]*storedLedger{}, timers: map[string]*time.Timer{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return s
	}
	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		key := strings.TrimSuffix(name, ".json")
		if !entry.Type().IsRegular() || key == name || !validKey(key) {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var stored storedLedger
		err = json.NewDecoder(io.LimitReader(f, storedLedgerLimit)).Decode(&stored)
		f.Close()
		if err != nil || stored.Key != key || !stored.Ledger.Clean(now) {
			continue
		}
		stored.Name = plainName(stored.Name, maxLedgerName)
		stored.sum, stored.savedMs = ledgerSum(stored.Ledger), stored.UpdatedMs
		s.devices[key] = &stored
	}
	return s
}

func validKey(key string) bool {
	return len(key) == 32 && strings.Trim(key, "0123456789abcdef") == ""
}

// Put keeps the ledger a paired computer sent for its install. paired says
// whether a pairing is still in place.
func (s *DeviceStore) Put(key, device, name string, ledger *Ledger, paired func(device string) bool, now time.Time) error {
	if !validKey(key) || device == "" {
		return errInvalidLedger
	}
	if !ledger.Clean(now) {
		return errInvalidLedger
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Ledgers of pairings that are gone, and the one this pairing sent for
	// another install, no longer count.
	for k, d := range s.devices {
		if k != key && (d.Device == device || !paired(d.Device)) {
			s.removeLocked(k)
		}
	}
	old, known := s.devices[key]
	if !known && len(s.devices) >= maxStoredDevices {
		return ErrStoreFull
	}
	stored := &storedLedger{Key: key, Device: device, Name: plainName(name, maxLedgerName), UpdatedMs: now.UnixMilli(), Ledger: ledger, sum: ledgerSum(ledger)}
	s.version++
	nowMs := now.UnixMilli()
	if known && old.Device == device && old.Name == stored.Name && old.sum == stored.sum {
		// The same usage: only the time it was sent changes, written now and
		// then (and at once when the last write is not on disk, or was
		// dated by a clock that ran ahead).
		stored.Ledger, stored.savedMs, stored.dirty, stored.failed = old.Ledger, old.savedMs, old.dirty, old.failed
		s.devices[key] = stored
		if s.closing || nowMs-old.savedMs >= unchangedSave.Milliseconds() || old.savedMs > nowMs || old.dirty && (old.failed || s.timers[key] == nil) {
			return s.writeLocked(key)
		}
		return nil
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if len(raw) > keptLedgerLimit {
		s.version--
		return ErrLedgerTooLarge
	}
	raw = nil
	if known {
		stored.savedMs = old.savedMs
	}
	s.devices[key] = stored
	// Written at once, or when writeEvery has passed since the last write;
	// until then the latest is kept here. A last write dated ahead of now,
	// by a clock that ran ahead, holds nothing up.
	wait := time.Duration(stored.savedMs+writeEvery.Milliseconds()-nowMs) * time.Millisecond
	if known && !s.closing && wait > 0 && stored.savedMs <= nowMs {
		stored.dirty = true
		s.retryLocked(key, min(wait, writeEvery))
		return nil
	}
	return s.writeLocked(key)
}

// retryLocked has a ledger kept in memory only written after wait.
func (s *DeviceStore) retryLocked(key string, wait time.Duration) {
	if s.timers[key] == nil {
		s.timers[key] = time.AfterFunc(wait, func() { s.flushKey(key) })
	}
}

// writeLocked writes a kept ledger to its file.
func (s *DeviceStore) writeLocked(key string) error {
	d := s.devices[key]
	if d == nil {
		return nil
	}
	raw, err := json.Marshal(d)
	if err == nil {
		err = writePrivateFile(filepath.Join(s.dir, key+".json"), raw)
	}
	if err != nil {
		// Kept in memory, and tried again: by a timer, Flush or the next
		// ledger sent.
		d.dirty, d.failed = true, true
		s.retryLocked(key, writeEvery)
		return err
	}
	d.savedMs, d.dirty, d.failed = d.UpdatedMs, false, false
	return nil
}

func (s *DeviceStore) flushKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.timers, key)
	if d := s.devices[key]; d != nil && d.dirty {
		if err := s.writeLocked(key); err != nil {
			log.Printf("usage: keeping a computer's usage: %v", err)
		}
	}
}

// Flush writes every ledger kept in memory only, as when Switcher stops;
// a ledger kept after it is written at once.
func (s *DeviceStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for key, d := range s.devices {
		if d.dirty {
			if err := s.writeLocked(key); err != nil {
				log.Printf("usage: keeping a computer's usage: %v", err)
			}
		}
	}
}

// Version changes whenever a kept ledger, or the time one was sent, does.
func (s *DeviceStore) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// LedgersAt is Ledgers with the version they are of, read together.
func (s *DeviceStore) LedgersAt(paired func(device string) bool) ([]DeviceLedger, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.ledgersLocked(paired)
	return out, s.version
}

// ledgerSum tells one ledger's usage from another's: its entries and
// sessions, not when it was scanned.
func ledgerSum(l *Ledger) [32]byte {
	raw, _ := json.Marshal(struct {
		E []LedgerEntry `json:"e"`
		S []SessionSpan `json:"s"`
	}{l.Entries, l.Sessions})
	return sha256.Sum256(raw)
}

func (s *DeviceStore) removeLocked(key string) {
	_ = os.Remove(filepath.Join(s.dir, key+".json"))
	delete(s.devices, key)
	if t := s.timers[key]; t != nil {
		t.Stop()
		delete(s.timers, key)
	}
	s.version++
}

// Ledgers are the kept ledgers of computers still paired, for adding up;
// those of pairings that are gone are removed.
func (s *DeviceStore) Ledgers(paired func(device string) bool) []DeviceLedger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ledgersLocked(paired)
}

func (s *DeviceStore) ledgersLocked(paired func(device string) bool) []DeviceLedger {
	out := make([]DeviceLedger, 0, len(s.devices))
	for k, d := range s.devices {
		if !paired(d.Device) {
			s.removeLocked(k)
			continue
		}
		out = append(out, DeviceLedger{Key: d.Key, Name: d.Name, UpdatedMs: d.UpdatedMs, Ledger: d.Ledger})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// writePrivateFile puts raw at path for this user alone, whole or not at all.
func writePrivateFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
