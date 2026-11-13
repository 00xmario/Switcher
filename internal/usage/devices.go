package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DeviceKey is the key a computer's ledger is kept under: a hash of its
// Switcher install's random id. It stays the same when the computer pairs
// again, so its usage is never counted twice, and the id itself is not
// passed on to other computers.
func DeviceKey(install string) string {
	sum := sha256.Sum256([]byte("switcher-usage|" + install))
	return hex.EncodeToString(sum[:16])
}

// ErrStoreFull refuses a computer's ledger when the host keeps as many as
// it can, all of computers still paired.
var ErrStoreFull = errors.New("this host keeps the usage of as many computers as it can")

// maxStoredDevices bounds how many computers' ledgers a host keeps.
const maxStoredDevices = 32

// storedLedgerLimit bounds a kept ledger's file: what a host reads back.
const storedLedgerLimit = 16 << 20

// keptLedgerLimit is the largest ledger Put keeps; tests lower it.
var keptLedgerLimit = storedLedgerLimit

// DeviceStore keeps, on a host, the ledgers its paired computers send: one
// per Switcher install, on disk, so a computer that is away still counts.
// Each pairing has one: a pairing that sends another install's ledger gives
// up the one it sent before. The install's id is the computer's own secret
// (the host passes on only its hash), so the pairing that sends it last
// holds its ledger: a computer that paired again, after an unpairing that
// never reached the host, keeps its numbers. A ledger counts while its
// pairing is in place; the owner removing the pairing removes it.
type DeviceStore struct {
	dir     string
	mu      sync.Mutex
	devices map[string]*storedLedger
}

type storedLedger struct {
	Key       string  `json:"key"`
	Device    string  `json:"device"` // the pairing that sent it last
	Name      string  `json:"name"`
	UpdatedMs int64   `json:"updated_ms"`
	Ledger    *Ledger `json:"ledger"`

	sum     [32]byte // of the ledger as sent on, to tell an unchanged one
	savedMs int64    // when the file was last written
}

// ErrLedgerTooLarge refuses a ledger larger than a host keeps.
var ErrLedgerTooLarge = errors.New("this computer's usage is larger than a host keeps")

// unchangedSave is how often an unchanged ledger's file is written again,
// so the time it was last sent survives a restart.
const unchangedSave = 30 * time.Minute

// OpenDeviceStore reads the ledgers kept in dir.
func OpenDeviceStore(dir string) *DeviceStore {
	s := &DeviceStore{dir: dir, devices: map[string]*storedLedger{}}
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
	// Unchanged and saved lately: only the time it was sent changes.
	if known && old.Device == device && old.Name == stored.Name && old.sum == stored.sum && now.UnixMilli()-old.savedMs < unchangedSave.Milliseconds() {
		stored.Ledger, stored.savedMs = old.Ledger, old.savedMs
		s.devices[key] = stored
		return nil
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if len(raw) > keptLedgerLimit {
		return ErrLedgerTooLarge
	}
	if err := writePrivateFile(filepath.Join(s.dir, key+".json"), raw); err != nil {
		return err
	}
	stored.savedMs = stored.UpdatedMs
	s.devices[key] = stored
	return nil
}

func ledgerSum(l *Ledger) [32]byte {
	raw, _ := json.Marshal(l)
	return sha256.Sum256(raw)
}

func (s *DeviceStore) removeLocked(key string) {
	_ = os.Remove(filepath.Join(s.dir, key+".json"))
	delete(s.devices, key)
}

// Ledgers are the kept ledgers of computers still paired, for adding up;
// those of pairings that are gone are removed.
func (s *DeviceStore) Ledgers(paired func(device string) bool) []DeviceLedger {
	s.mu.Lock()
	defer s.mu.Unlock()
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
