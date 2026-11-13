package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	// usageUploadEvery is how soon a pairing's usage is taken again after
	// the host kept it: it sends every few minutes, and at most once a
	// minute when asked. usageTryEvery is how soon it may try again after
	// an upload the host did not keep.
	usageUploadEvery = 50 * time.Second
	usageTryEvery    = 10 * time.Second
	// usageIOWait bounds reading an upload and writing an answer.
	usageIOWait = 30 * time.Second
)

// usageUploaders remembers, per pairing, whether it is sending, when it last
// tried and when the host last kept what it sent.
type usageUploaders struct {
	mu    sync.Mutex
	state map[string]usageUploader
}

type usageUploader struct {
	busy    bool
	tried   time.Time
	kept    time.Time
	refused time.Time // what it sent was turned down for what it was
}

// How an upload ended.
type uploadOutcome int

const (
	uploadKept    uploadOutcome = iota
	uploadRefused               // not a ledger the host keeps: no sooner again than a kept one
	uploadFailed                // the host could not keep it: tried again shortly
)

// Why an upload is not taken now.
type uploadRefusal int

const (
	uploadTaken  uploadRefusal = iota
	uploadRecent               // the host kept this pairing's usage a moment ago
	uploadBusy                 // one is under way, or one was tried a moment ago
)

// begin lets a pairing send: one upload at a time, not again soon after one
// was tried, and not again within usageUploadEvery of one the host kept.
func (u *usageUploaders) begin(device string, now time.Time) uploadRefusal {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state == nil {
		u.state = map[string]usageUploader{}
	}
	s := u.state[device]
	switch {
	case s.busy:
		return uploadBusy
	case now.Sub(s.kept) < usageUploadEvery:
		return uploadRecent
	case now.Sub(s.refused) < usageUploadEvery, now.Sub(s.tried) < usageTryEvery:
		return uploadBusy
	}
	s.busy, s.tried = true, now
	u.state[device] = s
	return uploadTaken
}

// end finishes an upload as it went.
func (u *usageUploaders) end(device string, outcome uploadOutcome, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.state[device]
	s.busy = false
	switch outcome {
	case uploadKept:
		s.kept = now
	case uploadRefused:
		s.refused = now
	}
	u.state[device] = s
}

// prune forgets pairings that are not sending and may send again.
func (u *usageUploaders) prune(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for device, s := range u.state {
		if !s.busy && now.Sub(s.tried) >= usageTryEvery && now.Sub(s.kept) >= usageUploadEvery && now.Sub(s.refused) >= usageUploadEvery {
			delete(u.state, device)
		}
	}
}

// handleUsageDevice keeps the ledger a paired computer sends: its own only.
func (a *API) handleUsageDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := remote.PairedDeviceID(r)
	name, _ := remote.PairedDevice(r)
	if !ok || a.UsageDevices == nil || !a.hostingUsage() {
		http.NotFound(w, r)
		return
	}
	switch a.usageUploaders.begin(device, time.Now()) {
	case uploadRecent:
		// Told apart from a host too busy to take it: what the host kept
		// of this computer's usage is current enough.
		w.Header().Set(remote.UsageRecentHeader, "recent")
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "this computer sent its usage a moment ago; it sends it again shortly"})
		return
	case uploadBusy:
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "this computer's usage is being sent already; try again in a moment"})
		return
	}
	outcome := uploadFailed
	defer func() { a.usageUploaders.end(device, outcome, time.Now()) }()
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(usageIOWait))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, usageUploadLimit))
	if err != nil {
		outcome = uploadRefused
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
		outcome = uploadRefused
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage ledger"})
		return
	}
	raw = nil
	err = a.UsageDevices.Put(usage.DeviceKey(body.ID), device, name, body.Ledger, a.RemoteHost.Paired, time.Now())
	switch {
	case errors.Is(err, usage.ErrStoreFull) || errors.Is(err, usage.ErrLedgerTooLarge):
		outcome = uploadRefused
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, usage.ErrInvalidLedger):
		outcome = uploadRefused
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage ledger"})
	case err != nil:
		log.Printf("usage: keeping a paired computer's usage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the host could not keep this computer's usage"})
	default:
		outcome = uploadKept
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// hostingUsage reports whether this computer shares its accounts, and so
// keeps and gives the usage of the computers paired with it.
func (a *API) hostingUsage() bool { return a.RemoteHost != nil && a.RemoteHost.Enabled() }

// usageAnswer is the last answer to GET /api/tokens/ledgers, kept while
// nothing in it changed: every paired computer asking is given the same
// bytes. A new one re-encodes only the ledgers that changed, and keeps the
// encoding of those that fit the answer, the size alone of the others.
type usageAnswer struct {
	once    sync.Once
	lock    chan struct{} // held while the answer is made; a request may stop waiting
	version uint64
	own     *usage.Ledger
	ownAt   time.Time
	key     string
	name    string
	ownPart []byte
	parts   map[string]answerPart
	body    []byte
}

type answerPart struct {
	ledger    *usage.Ledger // the ledger it is the encoding of: a new one is encoded again
	updatedMs int64
	name      string
	size      int
	raw       []byte // nil for one that did not fit, or was not kept
}

func (u *usageAnswer) init() { u.once.Do(func() { u.lock = make(chan struct{}, 1) }) }

// drop forgets the answer, unless one is being made.
func (u *usageAnswer) drop() {
	u.init()
	select {
	case u.lock <- struct{}{}:
		u.body, u.ownPart, u.parts, u.own = nil, nil, nil, nil
		<-u.lock
	default:
	}
}

// handleUsageLedgers gives a paired computer every computer's ledger: this
// host's own first, then the ones paired computers sent, the smallest
// first, as many as fit one answer.
func (a *API) handleUsageLedgers(w http.ResponseWriter, r *http.Request) {
	if _, ok := remote.PairedDeviceID(r); !ok || a.UsageDevices == nil || a.Usage == nil || !a.hostingUsage() {
		http.NotFound(w, r)
		return
	}
	body, err := a.usageLedgersAnswer(r.Context())
	if err != nil {
		return // the computer asking has gone
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(usageIOWait))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

func (a *API) usageLedgersAnswer(ctx context.Context) ([]byte, error) {
	u := &a.usageAnswer
	u.init()
	select {
	case u.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-u.lock }()
	// Read while the answer is held: an answer is never made from what is
	// older than the one before it.
	devices, version := a.UsageDevices.LedgersAt(a.RemoteHost.Paired)
	own, at := a.Usage.OwnLedger()
	key, name, _ := a.UsageIdentity()
	ownSame := u.own == own && u.ownAt.Equal(at) && u.key == key && u.name == name
	if u.body != nil && u.version == version && ownSame {
		return u.body, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if own == nil {
		go a.Usage.Rescan()
		u.ownPart = nil
	} else if !ownSame || u.ownPart == nil {
		raw, err := json.Marshal(usage.DeviceLedger{Key: key, Name: name, Host: true, UpdatedMs: at.UnixMilli(), Ledger: own.ForSending()})
		if err != nil {
			raw = nil
		}
		u.ownPart = raw
	}
	// Each computer's encoding, made again only when it changed; at most an
	// answer's worth of them is held at a time.
	parts := make(map[string]answerPart, len(devices))
	byKey := make(map[string]usage.DeviceLedger, len(devices))
	held := 0
	for _, d := range devices {
		byKey[d.Key] = d
		p, ok := u.parts[d.Key]
		if !ok || p.ledger != d.Ledger || p.updatedMs != d.UpdatedMs || p.name != d.Name {
			raw, err := json.Marshal(d)
			if err != nil {
				continue
			}
			p = answerPart{ledger: d.Ledger, updatedMs: d.UpdatedMs, name: d.Name, size: len(raw)}
			if held+len(raw) <= usageAnswerLimit {
				p.raw = raw
			}
		}
		held += len(p.raw)
		parts[d.Key] = p
	}
	keys := make([]string, 0, len(parts))
	for k := range parts {
		keys = append(keys, k)
	}
	// A large ledger is the one left out, not the ones it would crowd out.
	sort.Slice(keys, func(i, j int) bool {
		if parts[keys[i]].size != parts[keys[j]].size {
			return parts[keys[i]].size < parts[keys[j]].size
		}
		return keys[i] < keys[j]
	})
	var out bytes.Buffer
	out.WriteString(`{"devices":[`)
	written := 0
	// Where each part sits in the answer, and how long it is: kept as
	// slices of it, not as copies beside it.
	type span struct{ offset, size int }
	at0 := map[string]span{}
	add := func(raw []byte) int {
		if written > 0 {
			out.WriteByte(',')
		}
		offset := out.Len()
		out.Write(raw)
		written++
		return offset
	}
	room := usageAnswerLimit - len(`{"devices":[]}`+"\n")
	ownAt0 := -1
	if u.ownPart != nil && len(u.ownPart) <= room {
		ownAt0 = add(u.ownPart)
		room -= len(u.ownPart) + 1
	}
	for _, k := range keys {
		p := parts[k]
		if p.size+1 > room {
			continue
		}
		raw := p.raw
		if raw == nil {
			var err error
			if raw, err = json.Marshal(byKey[k]); err != nil {
				continue
			}
			// Measured as it is now: the room and the slice go by this.
			if p.size = len(raw); p.size+1 > room {
				parts[k] = p
				continue
			}
			parts[k] = p
		}
		at0[k] = span{add(raw), len(raw)}
		room -= len(raw) + 1
	}
	out.WriteString("]}\n")
	body := out.Bytes()
	for k, p := range parts {
		if at, in := at0[k]; in {
			p.raw, p.size = body[at.offset:at.offset+at.size:at.offset+at.size], at.size
		} else {
			p.raw = nil
		}
		parts[k] = p
	}
	if ownAt0 >= 0 {
		u.ownPart = body[ownAt0 : ownAt0+len(u.ownPart) : ownAt0+len(u.ownPart)]
	} else if u.ownPart != nil {
		u.ownPart = append([]byte(nil), u.ownPart...) // not a slice of the answer before
	}
	u.version, u.own, u.ownAt, u.key, u.name, u.parts, u.body = version, own, at, key, name, parts, body
	return u.body, nil
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
	case errors.Is(err, remote.ErrUsageSentRecently):
		return usageRetry
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
		// What a host keeps for its paired computers goes when it stops
		// sharing, and as they finish sending.
		if !a.hostingUsage() {
			a.usageAnswer.drop()
		}
		a.usageUploaders.prune(time.Now())
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
	if err := a.usageShare.err; err != nil && !errors.Is(err, remote.ErrUsageSentRecently) {
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
