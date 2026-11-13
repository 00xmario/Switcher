package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"switcher/internal/usage"
)

// Usage is added up across a host and the computers that use it: each sends
// the host its ledger (usage.Ledger) and the host gives each all of them, so
// every computer shows the same combined usage. The host keeps the ledgers
// of computers that are away; a client keeps the last ones it was given, so
// it shows them while the host is away.

// usageWait bounds one exchange with the host.
const usageWait = 20 * time.Second

// usageLimit bounds the ledgers a host gives; a host gives no more than
// 48 MiB of them.
const usageLimit = 64 << 20

// usageSendLimit is what a host takes: a ledger larger than it, which only
// very long model names could make, is sent with its oldest usage cut off.
const usageSendLimit = 16 << 20

type usageCache struct {
	mu      sync.Mutex
	loaded  bool
	ledgers []usage.DeviceLedger
	at      time.Time
	syncing bool
}

func (c *Client) usagePath() string { return filepath.Join(c.dir, "usage-ledgers.json") }

// Dir is the folder this client keeps its pairing and its copies in.
func (c *Client) Dir() string { return c.dir }

// InstallID is this Switcher install's random id, made on first use.
func (c *Client) InstallID() (string, error) { return c.installID() }

// ErrNoUsageSharing says the host does not add usage up: a release before
// usage was combined.
var ErrNoUsageSharing = errors.New("the host does not combine usage yet; update it")

// ErrUsageRefused wraps the host turning this computer's usage down.
var ErrUsageRefused = errors.New("the host refused")

// ErrUsageRejected is the host no longer knowing this computer's pairing:
// the usage it gave no longer counts here. It is an ErrUsageRefused.
var ErrUsageRejected = fmt.Errorf("%w: it no longer accepts this computer; pair again", ErrUsageRefused)

// SyncUsage sends the host this computer's ledger and takes the ledgers of
// all the computers it knows, which UsageLedgers then answers with; when
// the host does not take this computer's, it still gives the others'. It
// reaches the host also while this computer works without it: usage is not
// routing.
func (c *Client) SyncUsage(ctx context.Context, own *usage.Ledger) error {
	if !c.Paired() || own == nil {
		return nil
	}
	c.usage.mu.Lock()
	if c.usage.syncing {
		c.usage.mu.Unlock()
		return nil
	}
	c.usage.syncing = true
	c.usage.mu.Unlock()
	defer func() {
		c.usage.mu.Lock()
		c.usage.syncing = false
		c.usage.mu.Unlock()
	}()
	install, err := c.installID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, usageWait)
	defer cancel()
	pairing := c.pairingID()
	body, err := usageBody(install, own)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "https://switcher-host/api/tokens/device", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	sent := c.usageCall(req, nil)
	if errors.Is(sent, ErrUsageRejected) {
		c.forgetUsageOf(pairing)
		return sent
	}
	if errors.Is(sent, ErrNoUsageSharing) || errors.Is(sent, errUnreachable) {
		return sent
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://switcher-host/api/tokens/ledgers", nil)
	var answer struct {
		Devices usage.DeviceLedgers `json:"devices"`
	}
	if err := c.usageCall(req, &answer); err != nil {
		if errors.Is(err, ErrUsageRejected) {
			c.forgetUsageOf(pairing)
		}
		return err
	}
	now := time.Now()
	kept := make([]usage.DeviceLedger, 0, len(answer.Devices))
	for _, d := range answer.Devices {
		if len(d.Key) != 32 || !d.Ledger.Clean(now) {
			continue
		}
		d.Name = plainText(d.Name)
		kept = append(kept, d)
	}
	c.usage.mu.Lock()
	defer c.usage.mu.Unlock()
	// Paired elsewhere meanwhile: these are the earlier host's.
	if c.pairingID() != pairing {
		return nil
	}
	c.usage.ledgers, c.usage.at, c.usage.loaded = kept, now, true
	_ = writeCompactJSON(c.usagePath(), usageFile{At: now, Ledgers: kept})
	return sent
}

// usageBody is the request that sends this computer's ledger, within what
// the host takes: past it, the oldest half of what is left is cut off
// until it fits.
func usageBody(install string, own *usage.Ledger) ([]byte, error) {
	ledger := *own
	for {
		body, err := json.Marshal(map[string]any{"install": install, "ledger": &ledger})
		if err != nil || len(body) <= usageSendLimit {
			return body, err
		}
		if len(ledger.Entries) == 0 && len(ledger.Sessions) == 0 {
			return nil, errors.New("this computer's usage is too large to send")
		}
		ledger.Entries = ledger.Entries[len(ledger.Entries)/2:]
		sessions := append([]usage.SessionSpan(nil), ledger.Sessions...)
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].Last > sessions[j].Last })
		ledger.Sessions = sessions[:len(sessions)/2]
	}
}

type usageFile struct {
	At      time.Time           `json:"at"`
	Ledgers usage.DeviceLedgers `json:"ledgers"`
}

// errUnreachable wraps an exchange that never reached the host.
var errUnreachable = errors.New("could not reach the host")

func (c *Client) usageCall(req *http.Request, out any) error {
	resp, err := c.send(req, true)
	if err != nil {
		return fmt.Errorf("%w: %w", errUnreachable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.Header.Get(rejectedHeader) != "":
		return ErrUsageRejected
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoUsageSharing
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		return fmt.Errorf("the host is busy (%d); trying again soon", resp.StatusCode)
	case resp.StatusCode/100 != 2:
		var e struct {
			Error string `json:"error"`
		}
		_ = decodeOne(resp.Body, 8<<10, &e)
		return fmt.Errorf("%w: %s", ErrUsageRefused, plainText(e.Error))
	case out == nil:
		return nil
	}
	return decodeOne(resp.Body, usageLimit, out)
}

// UsageLedgers are the ledgers the host last gave, kept on disk so they
// count also while the host is away, and when they were given.
func (c *Client) UsageLedgers() ([]usage.DeviceLedger, time.Time) {
	if !c.Paired() {
		return nil, time.Time{}
	}
	c.usage.mu.Lock()
	defer c.usage.mu.Unlock()
	if !c.usage.loaded {
		c.usage.loaded = true
		var f usageFile
		if readJSON(c.usagePath(), &f) == nil {
			now := time.Now()
			for _, d := range f.Ledgers {
				if len(d.Key) == 32 && d.Ledger.Clean(now) {
					d.Name = plainText(d.Name)
					c.usage.ledgers = append(c.usage.ledgers, d)
				}
			}
			c.usage.at = f.At
		}
	}
	return c.usage.ledgers, c.usage.at
}

// forgetUsageOf is forgetUsage while pairing is still the current one.
func (c *Client) forgetUsageOf(pairing string) {
	if c.pairingID() == pairing {
		c.forgetUsage()
	}
}

// forgetUsage drops the ledgers of a host this computer no longer uses.
func (c *Client) forgetUsage() {
	c.usage.mu.Lock()
	defer c.usage.mu.Unlock()
	c.usage.ledgers, c.usage.at, c.usage.loaded = nil, time.Time{}, true
	_ = os.Remove(c.usagePath())
}

// PairingID names the current pairing; it changes when this computer pairs
// again, also with the same host.
func (c *Client) PairingID() string { return c.pairingID() }

// pairingID names the current pairing, so an answer from an earlier host is
// not taken for the current one's.
func (c *Client) pairingID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil {
		return ""
	}
	return c.cfg.DeviceID + "|" + c.cfg.Fingerprint
}
