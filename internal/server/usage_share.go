package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"switcher/internal/remote"
	"switcher/internal/usage"
)

// Usage is added up across a host and the computers that use it; see
// internal/remote/usage.go. On a host, paired computers send their ledgers
// here and read everyone's; on a client, a background loop exchanges them,
// and the Usage page shows the sum.

// usageShare remembers, on a client, how the last exchange went.
type usageShare struct {
	mu     sync.Mutex
	last   time.Time // the last exchange tried
	err    error     // what it failed with, if it did
	paired bool      // paired at the last look, so a new pairing exchanges at once
}

const (
	// usageSyncEvery is how often a client sends its usage and takes
	// everyone's; a client whose exchange failed tries again after
	// usageRetry, and the Usage page asks for one after usagePageSync.
	usageSyncEvery = 5 * time.Minute
	usageRetry     = time.Minute
	usagePageSync  = time.Minute
)

// handleUsageDevice keeps the ledger a paired computer sends: its own only.
func (a *API) handleUsageDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := remote.PairedDeviceID(r)
	name, _ := remote.PairedDevice(r)
	if !ok || a.UsageDevices == nil || a.RemoteHost == nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Install string        `json:"install"`
		Ledger  *usage.Ledger `json:"ledger"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&body); err != nil || !remote.ValidInstallID(body.Install) || body.Ledger == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage ledger"})
		return
	}
	err := a.UsageDevices.Put(usage.DeviceKey(body.Install), device, name, body.Ledger, a.RemoteHost.Paired, time.Now())
	switch {
	case errors.Is(err, usage.ErrClaimed):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// handleUsageLedgers gives a paired computer every computer's ledger: this
// host's own and the ones paired computers sent.
func (a *API) handleUsageLedgers(w http.ResponseWriter, r *http.Request) {
	if _, ok := remote.PairedDeviceID(r); !ok || a.UsageDevices == nil || a.Usage == nil {
		http.NotFound(w, r)
		return
	}
	devices := a.UsageDevices.Ledgers()
	if own, at := a.Usage.OwnLedger(); own != nil {
		key, name, _ := a.UsageIdentity()
		devices = append([]usage.DeviceLedger{{Key: key, Name: name, Host: true, UpdatedMs: at.UnixMilli(), Ledger: own}}, devices...)
	} else {
		go a.Usage.Scan(30)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// UsageIdentity is this computer among the ones whose usage is added up.
func (a *API) UsageIdentity() (key, name string, host bool) {
	key, name = "this", "This computer"
	if a.RemoteClient != nil {
		if install, err := a.RemoteClient.InstallID(); err == nil {
			key = usage.DeviceKey(install)
		}
	}
	if a.MachineName != "" {
		name = a.MachineName
	}
	return key, name, a.RemoteHost != nil && a.RemoteHost.Enabled()
}

// UsageLedgers are the other computers' ledgers to add to this one's: those
// paired computers sent this host, and those this computer's own host gave.
func (a *API) UsageLedgers() []usage.DeviceLedger {
	var out []usage.DeviceLedger
	seen := map[string]int{}
	add := func(d usage.DeviceLedger) {
		if i, ok := seen[d.Key]; ok {
			if d.UpdatedMs > out[i].UpdatedMs {
				out[i] = d
			}
			return
		}
		seen[d.Key] = len(out)
		out = append(out, d)
	}
	if a.RemoteClient != nil {
		ledgers, _ := a.RemoteClient.UsageLedgers()
		for _, d := range ledgers {
			add(d)
		}
	}
	if a.UsageDevices != nil {
		for _, d := range a.UsageDevices.Ledgers() {
			add(d)
		}
	}
	return out
}

// syncUsage, on a computer paired with a host, sends its usage and takes
// everyone's.
func (a *API) syncUsage(ctx context.Context) {
	if a.RemoteClient == nil || a.Usage == nil || !a.RemoteClient.Paired() {
		return
	}
	own := a.Usage.Own()
	if own == nil {
		a.Usage.Scan(30)
		if own = a.Usage.Own(); own == nil {
			return
		}
	}
	a.usageShare.mu.Lock()
	a.usageShare.last = time.Now()
	a.usageShare.mu.Unlock()
	err := a.RemoteClient.SyncUsage(ctx, own)
	a.usageShare.mu.Lock()
	a.usageShare.err = err
	a.usageShare.mu.Unlock()
}

// syncUsageIfOld exchanges usage when the last exchange is older than age.
func (a *API) syncUsageIfOld(age time.Duration) {
	a.usageShare.mu.Lock()
	due := time.Since(a.usageShare.last) >= age
	a.usageShare.mu.Unlock()
	if due {
		a.syncUsage(context.Background())
	}
}

// RunUsageSharing keeps, on a computer paired with a host, the combined
// usage current: it rescans this computer's logs and exchanges ledgers with
// the host every few minutes, at once on a new pairing, and again soon after
// a failed exchange. It returns when ctx ends.
func (a *API) RunUsageSharing(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		paired := a.RemoteClient != nil && a.RemoteClient.Paired()
		a.usageShare.mu.Lock()
		wait := usageSyncEvery
		if a.usageShare.err != nil {
			wait = usageRetry
		}
		due := paired && (!a.usageShare.paired || time.Since(a.usageShare.last) >= wait)
		a.usageShare.paired = paired
		a.usageShare.mu.Unlock()
		if due {
			a.Usage.RefreshStale([]int{30}, usageSyncEvery)
			a.syncUsage(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// usageSharing says, on a client, whose usage the Usage page adds up and
// how current the host's part is.
func (a *API) usageSharing() map[string]any {
	if a.RemoteClient == nil || !a.RemoteClient.Paired() {
		return nil
	}
	_, at := a.RemoteClient.UsageLedgers()
	out := map[string]any{"host": a.RemoteClient.Status().HostName}
	if !at.IsZero() {
		out["updated_ms"] = at.UnixMilli()
	}
	a.usageShare.mu.Lock()
	if err := a.usageShare.err; err != nil {
		out["error"] = err.Error()
	}
	a.usageShare.mu.Unlock()
	return out
}
