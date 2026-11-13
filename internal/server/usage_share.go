package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
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
)

// usageAnswerLimit bounds the ledgers a host gives at once: a client takes
// up to remote's usageLimit.
var usageAnswerLimit = 48 << 20

// usageUploads bounds how many ledgers from paired computers are decoded at
// once; each is read in full first, so a slow sender holds no slot.
var usageUploads = make(chan struct{}, 2)

// usageTick is how often the sharing loop looks whether an exchange is due.
var usageTick = 30 * time.Second

const (
	// usageUploadEvery is how soon a pairing may send its usage again: it
	// sends every few minutes, and at most once a minute when asked.
	usageUploadEvery = 50 * time.Second
	// usageIOWait bounds reading an upload and writing an answer.
	usageIOWait = 30 * time.Second
)

// usageUploaders remembers, per pairing, whether it is sending and when it
// last began to.
type usageUploaders struct {
	mu    sync.Mutex
	state map[string]usageUploader
}

type usageUploader struct {
	busy bool
	last time.Time
}

// begin lets a pairing send, one upload at a time and not too often.
func (u *usageUploaders) begin(device string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state == nil {
		u.state = map[string]usageUploader{}
	}
	s := u.state[device]
	if s.busy || now.Sub(s.last) < usageUploadEvery {
		return false
	}
	u.state[device] = usageUploader{busy: true, last: now}
	return true
}

func (u *usageUploaders) end(device string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.state[device]
	s.busy = false
	u.state[device] = s
}

// handleUsageDevice keeps the ledger a paired computer sends: its own only.
func (a *API) handleUsageDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := remote.PairedDeviceID(r)
	name, _ := remote.PairedDevice(r)
	if !ok || a.UsageDevices == nil || !a.hostingUsage() {
		http.NotFound(w, r)
		return
	}
	if !a.usageUploaders.begin(device, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "this computer sent its usage a moment ago; it sends it again shortly"})
		return
	}
	defer a.usageUploaders.end(device)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(usageIOWait))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, usageUploadLimit))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage ledger"})
		return
	}
	select {
	case usageUploads <- struct{}{}:
		defer func() { <-usageUploads }()
	case <-r.Context().Done():
		return
	}
	var body struct {
		ID     string        `json:"id"`
		Ledger *usage.Ledger `json:"ledger"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || !remote.ValidInstallID(body.ID) || body.Ledger == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage ledger"})
		return
	}
	raw = nil
	err = a.UsageDevices.Put(usage.DeviceKey(body.ID), device, name, body.Ledger, a.RemoteHost.Paired, time.Now())
	switch {
	case errors.Is(err, usage.ErrStoreFull) || errors.Is(err, usage.ErrLedgerTooLarge):
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

// usageAnswer is the last answer to GET /api/tokens/ledgers, kept while
// nothing in it changed: every paired computer asking is given the same
// bytes, made once.
type usageAnswer struct {
	mu   sync.Mutex
	sig  string
	body []byte
}

// handleUsageLedgers gives a paired computer every computer's ledger: this
// host's own first, then the ones paired computers sent, the smallest
// first, as many as fit one answer.
func (a *API) handleUsageLedgers(w http.ResponseWriter, r *http.Request) {
	if _, ok := remote.PairedDeviceID(r); !ok || a.UsageDevices == nil || a.Usage == nil || !a.hostingUsage() {
		http.NotFound(w, r)
		return
	}
	body := a.usageLedgersAnswer()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(usageIOWait))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

func (a *API) usageLedgersAnswer() []byte {
	devices := a.UsageDevices.Ledgers(a.RemoteHost.Paired)
	own, at := a.Usage.OwnLedger()
	if own == nil {
		go a.Usage.Rescan()
	}
	key, name, _ := a.UsageIdentity()
	var sig strings.Builder
	fmt.Fprintf(&sig, "%p|%d|%s|%s", own, at.UnixNano(), key, name)
	for _, d := range devices {
		fmt.Fprintf(&sig, "|%s:%d:%s", d.Key, d.UpdatedMs, d.Name)
	}
	a.usageAnswer.mu.Lock()
	defer a.usageAnswer.mu.Unlock()
	if a.usageAnswer.body != nil && a.usageAnswer.sig == sig.String() {
		return a.usageAnswer.body
	}
	type encoded struct{ raw []byte }
	var parts []encoded
	for _, d := range devices {
		if raw, err := json.Marshal(d); err == nil {
			parts = append(parts, encoded{raw})
		}
	}
	// A large ledger is the one left out, not the ones it would crowd out.
	sort.SliceStable(parts, func(i, j int) bool { return len(parts[i].raw) < len(parts[j].raw) })
	if own != nil {
		if raw, err := json.Marshal(usage.DeviceLedger{Key: key, Name: name, Host: true, UpdatedMs: at.UnixMilli(), Ledger: own.ForSending()}); err == nil {
			parts = append([]encoded{{raw}}, parts...)
		}
	}
	var out bytes.Buffer
	out.WriteString(`{"devices":[`)
	written := 0
	for _, p := range parts {
		if out.Len()+len(p.raw)+3 > usageAnswerLimit {
			continue
		}
		if written > 0 {
			out.WriteByte(',')
		}
		out.Write(p.raw)
		written++
	}
	out.WriteString("]}\n")
	a.usageAnswer.sig, a.usageAnswer.body = sig.String(), out.Bytes()
	return a.usageAnswer.body
}

// thisUsageKey stands in for this computer's key when it has no usage id.
var thisUsageKey = sync.OnceValue(func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
})

// UsageIdentity is this computer among the ones whose usage is added up.
func (a *API) UsageIdentity() (key, name string, host bool) {
	key, name = thisUsageKey(), "This computer"
	if a.RemoteClient != nil {
		if id, err := a.RemoteClient.UsageID(); err == nil {
			key = usage.DeviceKey(id)
		}
	}
	if a.MachineName != "" {
		name = a.MachineName
	}
	return key, name, a.hostingUsage()
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
