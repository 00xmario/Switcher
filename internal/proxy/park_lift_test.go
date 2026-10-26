package proxy

import (
	"context"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// parkUsageProvider answers usage checks with a fixed reading. During
// lets a test act while a check is in flight.
type parkUsageProvider struct {
	*fakeProvider
	usage  provider.Usage
	during func()
}

func (p *parkUsageProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	if p.during != nil {
		p.during()
	}
	return p.usage, nil
}

func monthly(used int) provider.Usage {
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{
		{Label: "Monthly", UsedPercent: used, ResetsAt: time.Now().Add(30 * 24 * time.Hour).Unix()}}}
}

func TestAReadingWithRoomLiftsAnEarlierPark(t *testing.T) {
	until := time.Now().Add(27 * 24 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		usage provider.Usage
		lift  bool
	}{
		{"room in every window", monthly(0), true},
		{"a window at the limit", monthly(100), false},
		{"the provider refuses", func() provider.Usage { u := monthly(0); u.Refused = true; return u }(), false},
		{"no windows", provider.Usage{Available: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := account("go")
			m := newManager(t, nil, a)
			m.providers["fake"] = &parkUsageProvider{fakeProvider: &fakeProvider{id: "fake"}, usage: tc.usage}
			if err := m.markExhausted(a.ID, until); err != nil {
				t.Fatal(err)
			}
			m.refreshAccount(context.Background(), a.ID, m.generation[a.ID], true)
			if _, parked := m.Exhausted(a.ID); parked == tc.lift {
				t.Fatalf("parked=%v after the reading, want lifted=%v", parked, tc.lift)
			}
		})
	}
}

func TestAParkDuringTheCheckIsKept(t *testing.T) {
	a := account("go")
	m := newManager(t, nil, a)
	until := time.Now().Add(27 * 24 * time.Hour).Truncate(time.Second)
	p := &parkUsageProvider{fakeProvider: &fakeProvider{id: "fake"}, usage: monthly(0)}
	p.during = func() {
		if err := m.markExhausted(a.ID, until); err != nil {
			t.Error(err)
		}
	}
	m.providers["fake"] = p
	m.refreshAccount(context.Background(), a.ID, m.generation[a.ID], true)
	if _, parked := m.Exhausted(a.ID); !parked {
		t.Fatal("a check that began before the park lifted it")
	}

	// Parked before the check, then parked again with a new reset during it.
	p.during = func() {
		if err := m.markExhausted(a.ID, until.Add(time.Hour)); err != nil {
			t.Error(err)
		}
	}
	m.refreshAccount(context.Background(), a.ID, m.generation[a.ID], true)
	if got, parked := m.Exhausted(a.ID); !parked || !got.Equal(until.Add(time.Hour)) {
		t.Fatal("a park renewed during the check was lifted")
	}
}
