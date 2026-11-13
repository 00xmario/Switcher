package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
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
	mu      sync.Mutex
	last    time.Time // the last exchange tried
	err     error     // what it failed with, if it did
	pairing string    // the pairing at the last look, so a new one exchanges at once
}

const (
	// usageSyncEvery is how often a client sends its usage and takes
	// everyone's; a client whose exchange failed tries again after
	// usageRetry, or usageRefusedRetry when the host turned it down, and
	// the Usage page asks for one after usagePageSync.
	usageSyncEvery    = 5 * time.Minute
	usageRetry        = time.Minute
	usageRefusedRetry = time.Hour
	usagePageSync     = time.Minute
	// usageUploadLimit bounds a ledger a paired computer sends; the largest
	// a ledger may be is well within it.
	usageUploadLimit = 16 << 20
	// usageAnswerLimit bounds the ledgers a host gives at once: a client
	// takes up to remote's usageLimit.
	usageAnswerLimit = 48 << 20
)

// usageUploads bounds how many ledgers from paired computers are read at
// once.
var usageUploads = make(chan struct{}, 2)

// usageTick is how often the sharing loop looks whether an exchange is due.
var usageTick = 30 * time.Second

// handleUsageDevice keeps the ledger a paired computer sends: its own only.
func (a *API) handleUsageDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := remote.PairedDeviceID(r)
	name, _ := remote.PairedDevice(r)
	if !ok || a.UsageDevices == nil || !a.hostingUsage() {
		http.NotFound(w, r)
		return
	}
	select {
	case usageUploads <- struct{}{}:
		defer func() { <-usageUploads }()
	default:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "the host is reading other computers' usage; try again in a moment"})
		return
	}
	var body struct {
		Install string        `json:"install"`
		Ledger  *usage.Ledger `json:"ledger"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, usageUploadLimit)).Decode(&body); err != nil || !remote.ValidInstallID(body.Install) || body.Ledger == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage ledger"})
		return
	}
	err := a.UsageDevices.Put(usage.DeviceKey(body.Install), device, name, body.Ledger, a.RemoteHost.Paired, time.Now())
	switch {
	case errors.Is(err, usage.ErrStoreFull):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		log.Printf("usage: keeping a paired computer's usage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the host could not keep this computer's usage"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// hostingUsage reports whether this computer shares its accounts, and so
// keeps and gives the usage of the computers paired with it.
func (a *API) hostingUsage() bool { return a.RemoteHost != nil && a.RemoteHost.Enabled() }

// handleUsageLedgers gives a paired computer every computer's ledger: this
// host's own and the ones paired computers sent, the most recent first, as
// many as fit one answer.
func (a *API) handleUsageLedgers(w http.ResponseWriter, r *http.Request) {
	if _, ok := remote.PairedDeviceID(r); !ok || a.UsageDevices == nil || a.Usage == nil || !a.hostingUsage() {
		http.NotFound(w, r)
		return
	}
	devices := a.UsageDevices.Ledgers(a.RemoteHost.Paired)
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].UpdatedMs > devices[j].UpdatedMs })
	if own, at := a.Usage.OwnLedger(); own != nil {
		key, name, _ := a.UsageIdentity()
		devices = append([]usage.DeviceLedger{{Key: key, Name: name, Host: true, UpdatedMs: at.UnixMilli(), Ledger: own}}, devices...)
	} else {
		go a.Usage.Rescan()
	}
	var out bytes.Buffer
	out.WriteString(`{"devices":[`)
	written := 0
	for _, d := range devices {
		raw, err := json.Marshal(d)
		if err != nil || out.Len()+len(raw)+3 > usageAnswerLimit {
			continue
		}
		if written > 0 {
			out.WriteByte(',')
		}
		out.Write(raw)
		written++
	}
	out.WriteString("]}\n")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out.Bytes())
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
	// A computer that no longer shares counts no computers paired with it.
	if a.UsageDevices != nil && a.hostingUsage() {
		for _, d := range a.UsageDevices.Ledgers(a.RemoteHost.Paired) {
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
		a.Usage.Rescan()
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

// syncUsageIfOld exchanges usage when the last exchange is older than age,
// or than the wait after the host turned one down.
func (a *API) syncUsageIfOld(age time.Duration) {
	a.usageShare.mu.Lock()
	due := time.Since(a.usageShare.last) >= max(age, a.usageWaitLocked(0))
	a.usageShare.mu.Unlock()
	if due {
		a.syncUsage(context.Background())
	}
}

// usageWaitLocked is how long after the last exchange the next is due:
// every, after one that went well; soon after one that failed on the way;
// an hour after one the host turned down or does not know.
func (a *API) usageWaitLocked(every time.Duration) time.Duration {
	switch err := a.usageShare.err; {
	case err == nil:
		return every
	case errors.Is(err, remote.ErrNoUsageSharing) || errors.Is(err, remote.ErrUsageRefused):
		return usageRefusedRetry
	default:
		return usageRetry
	}
}

// RunUsageSharing keeps, on a computer paired with a host, the combined
// usage current: it rescans this computer's logs and exchanges ledgers with
// the host every few minutes, at once on a new pairing, and again soon after
// a failed exchange. It returns when ctx ends.
func (a *API) RunUsageSharing(ctx context.Context) {
	tick := time.NewTicker(usageTick)
	defer tick.Stop()
	for {
		pairing := ""
		if a.RemoteClient != nil && a.RemoteClient.Paired() {
			pairing = a.RemoteClient.PairingID()
		}
		a.usageShare.mu.Lock()
		wait := a.usageWaitLocked(usageSyncEvery)
		// A computer that paired, or paired again, exchanges at once: the
		// host counts it only from its new pairing on.
		due := pairing != "" && (pairing != a.usageShare.pairing || time.Since(a.usageShare.last) >= wait)
		a.usageShare.pairing = pairing
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
		switch {
		case errors.Is(err, remote.ErrNoUsageSharing):
			out["reason"] = "old_host"
		case errors.Is(err, remote.ErrUsageRejected):
			out["reason"] = "rejected"
		case errors.Is(err, remote.ErrUsageRefused):
			out["reason"] = "refused"
		default:
			out["reason"] = "unreachable"
		}
	}
	a.usageShare.mu.Unlock()
	return out
}
