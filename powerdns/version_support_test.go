package powerdns

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Feature availability differs across PowerDNS releases and backends, so the
// tests that depend on one ask the server rather than assuming. This keeps a
// run against an older server meaningful instead of failing on things that
// release genuinely cannot do.
var (
	serverCapsOnce sync.Once
	serverCaps     struct {
		views    bool
		comments bool
		catalog  bool
	}
)

// detectServerCapabilities probes the running authoritative server once.
func detectServerCapabilities(t *testing.T) {
	t.Helper()

	serverCapsOnce.Do(func() {
		serverURL := os.Getenv("PDNS_SERVER_URL")
		apiKey := os.Getenv("PDNS_API_KEY")
		if serverURL == "" || apiKey == "" {
			return
		}

		client, err := NewPowerDNSClient(context.Background(), serverURL, os.Getenv("PDNS_SERVER_ID"), apiKey, nil, false, "0", 0, 0)
		if err != nil {
			return
		}

		// Views and networks were added in PowerDNS 5.0, and even where the
		// endpoint exists the server rejects writes unless it runs the LMDB
		// backend with views enabled and a zone cache. Listing therefore is not
		// enough to tell whether these resources can actually be managed, so
		// this probes with a real write and undoes it.
		serverCaps.views = probeViewSupport(client)

		// Record comments need a backend that stores them. LMDB only gained
		// that in 5.1, and the API reports it as a 422 on the write itself, so
		// this probes with a real zone.
		serverCaps.comments = probeCommentSupport(client)
		serverCaps.catalog = probeCatalogSupport(client)
	})
}

// probeViewSupport tries an actual network write, which is what the resources
// do. A server that lists views but cannot accept one is no use to them.
func probeViewSupport(client *PowerDNSClient) bool {
	ctx := context.Background()

	req, err := client.newRequest(ctx, http.MethodGet, client.serverEndpoint("/views"), nil)
	if err != nil {
		return false
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	const probeNetwork = "198.51.100.0"
	const probePrefix = "24"
	if err := client.SetNetwork(ctx, probeNetwork, probePrefix, "capability-probe"); err != nil {
		return false
	}
	_ = client.DeleteNetwork(ctx, probeNetwork, probePrefix)

	return true
}

// probeCommentSupport creates a throwaway zone and tries to attach a comment.
func probeCommentSupport(client *PowerDNSClient) bool {
	ctx := context.Background()
	zone := "capability-probe.invalid."

	// Ignore failures here: a leftover zone from an interrupted run is fine,
	// and a create failure means the probe simply reports no support.
	_ = client.DeleteZone(ctx, zone)
	if _, err := client.CreateZone(ctx, ZoneInfo{Name: zone, Kind: "Native", Nameservers: []string{}}); err != nil {
		return false
	}
	defer func() { _ = client.DeleteZone(ctx, zone) }()

	comments := []Comment{{Content: "capability probe"}}
	_, err := client.ReplaceRecordSet(ctx, zone, ResourceRecordSet{
		Name:       "probe." + zone,
		Type:       "A",
		TTL:        300,
		ChangeType: "REPLACE",
		Records:    []Record{{Name: "probe." + zone, Type: "A", Content: "192.0.2.1", TTL: 300}},
		Comments:   &comments,
	})

	return err == nil
}

// probeCatalogSupport checks whether the server keeps a catalog assignment.
// PowerDNS before 4.7 accepts the field on create and then drops it, so asking
// for it back is the only reliable signal.
func probeCatalogSupport(client *PowerDNSClient) bool {
	ctx := context.Background()
	zone := "catalog-probe.invalid."

	_ = client.DeleteZone(ctx, zone)
	created, err := client.CreateZone(ctx, ZoneInfo{
		Name:        zone,
		Kind:        "Master",
		Catalog:     "catalog-probe-parent.invalid.",
		Nameservers: []string{},
	})
	if err != nil {
		return false
	}
	defer func() { _ = client.DeleteZone(ctx, zone) }()

	return created.Catalog != ""
}

// testAccPreCheckCatalog skips a test when the server drops catalog zones.
func testAccPreCheckCatalog(t *testing.T) {
	t.Helper()
	testAccPreCheck(t)
	detectServerCapabilities(t)
	if !serverCaps.catalog {
		t.Skip("server does not support catalog zones (PowerDNS 4.7+)")
	}
}

// testAccPreCheckViews skips a test when the server has no views support.
func testAccPreCheckViews(t *testing.T) {
	t.Helper()
	testAccPreCheck(t)
	detectServerCapabilities(t)
	if !serverCaps.views {
		t.Skip("server does not support views and networks (needs PowerDNS 5.0+ on a backend that provides them, such as LMDB)")
	}
}

// testAccPreCheckComments skips a test when the backend cannot store comments.
func testAccPreCheckComments(t *testing.T) {
	t.Helper()
	testAccPreCheck(t)
	detectServerCapabilities(t)
	if !serverCaps.comments {
		t.Skip("backend does not store record comments (LMDB gained these in PowerDNS 5.1; gpgsql supports them)")
	}
}

// testAccPreCheckRecursorHostnames skips a test when the Recursor only accepts
// IP addresses as forwarders. Recursor 5.1 does: a host name in a forward zone
// is refused when the zone is created, and the Recursor then fails to start
// because the name was saved. 5.3 resolves host names. Asking the server by
// writing one would leave that bad entry behind, so this reads the version.
func testAccPreCheckRecursorHostnames(t *testing.T) {
	t.Helper()
	testAccPreCheckRecursor(t)

	serverID := os.Getenv("PDNS_RECURSOR_SERVER_ID")
	if serverID == "" {
		serverID = "localhost"
	}
	url := strings.TrimRight(os.Getenv("PDNS_RECURSOR_SERVER_URL"), "/") + "/api/v1/servers/" + serverID
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the recursor version request: %v", err)
	}
	req.Header.Set("X-API-Key", os.Getenv("PDNS_API_KEY"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reading the recursor version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var info struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decoding the recursor version: %v", err)
	}
	major, minor, ok := parseMajorMinor(info.Version)
	if !ok {
		t.Fatalf("cannot read the recursor version %q", info.Version)
	}
	if major < 5 || (major == 5 && minor < 3) {
		t.Skipf("Recursor %s accepts only IP addresses as forwarders; host names need 5.3 or later", info.Version)
	}
}

// parseMajorMinor reads the first two numbers of a version such as "5.1.10".
func parseMajorMinor(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	return major, minor, errMajor == nil && errMinor == nil
}

func TestParseMajorMinor(t *testing.T) {
	for version, want := range map[string][2]int{"5.1.10": {5, 1}, "5.4.0": {5, 4}, "4.9": {4, 9}} {
		major, minor, ok := parseMajorMinor(version)
		if !ok || major != want[0] || minor != want[1] {
			t.Errorf("parseMajorMinor(%q) = %d.%d, %v; want %d.%d", version, major, minor, ok, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "5", "x.y.z"} {
		if _, _, ok := parseMajorMinor(bad); ok {
			t.Errorf("parseMajorMinor(%q) should fail", bad)
		}
	}
}
