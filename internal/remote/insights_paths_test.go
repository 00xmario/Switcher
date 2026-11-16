package remote

import "testing"

// The accounts' quota history is the host's, so a computer that uses a host
// asks it, and the host lets a paired computer read it; Insights about
// usage is worked out on each computer from the ledgers it has.
func TestQuotaHistoryComesFromTheHost(t *testing.T) {
	if !forwarded("/api/insights/limits") || !allowedRemotePath("/api/insights/limits") {
		t.Fatal("the quota history is not asked of the host")
	}
	if forwarded("/api/insights") || allowedRemotePath("/api/insights") {
		t.Fatal("usage insights are sent to the host, which works them out from its own ledgers only")
	}
}
