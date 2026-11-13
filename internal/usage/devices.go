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

// ErrClaimed refuses a ledger for an install another paired computer sends.
var ErrClaimed = errors.New("another paired computer sends the usage of that Switcher")

// maxStoredDevices bounds how many computers' ledgers a host keeps.
const maxStoredDevices = 64

// DeviceStore keeps, on a host, the ledgers its paired computers send: one
// per Switcher install, on disk, so a computer that is away still counts.
// A paired computer may write only the ledger it claimed first, while it
// stays paired: no other can change its numbers.
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
}

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
		err = json.NewDecoder(io.LimitReader(f, 64<<20)).Decode(&stored)
		f.Close()
		if err != nil || stored.Key != key || !stored.Ledger.Clean(now) {
			continue
		}
		stored.Name = plainName(stored.Name, maxLedgerName)
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
		return errors.New("invalid usage ledger")
	}
	if !ledger.Clean(now) {
		return errors.New("invalid usage ledger")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.devices[key]; old != nil && old.Device != device && paired(old.Device) {
		return ErrClaimed
	}
	if _, known := s.devices[key]; !known && len(s.devices) >= maxStoredDevices {
		// Make room by the ledger longest unchanged of a computer no longer
		// paired; with every one still paired, there is none to give up.
		oldest := ""
		for k, d := range s.devices {
			if !paired(d.Device) && (oldest == "" || d.UpdatedMs < s.devices[oldest].UpdatedMs) {
				oldest = k
			}
		}
		if oldest == "" {
			return errors.New("this host keeps the usage of as many computers as it can")
		}
		_ = os.Remove(filepath.Join(s.dir, oldest+".json"))
		delete(s.devices, oldest)
	}
	stored := &storedLedger{Key: key, Device: device, Name: plainName(name, maxLedgerName), UpdatedMs: now.UnixMilli(), Ledger: ledger}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(s.dir, key+".json"), raw); err != nil {
		return err
	}
	s.devices[key] = stored
	return nil
}

// Ledgers are the kept ledgers, for adding up.
func (s *DeviceStore) Ledgers() []DeviceLedger {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeviceLedger, 0, len(s.devices))
	for _, d := range s.devices {
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
