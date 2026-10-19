package webui

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/duffel"
)

const testTenantID = "tn__01M25DB6ARSVP2BVHBZFWA95SE"

// Tenant IDs are minted with uppercase ULIDs while Duffel directory names are
// lowercased when they are created. A prefix built from the raw ID matches
// nothing, which made every console search return zero results.
func TestTenantSearchPrefixLowercasesTheTenant(t *testing.T) {
	prefix, err := tenantSearchPrefix(testTenantID)
	if err != nil {
		t.Fatalf("tenantSearchPrefix: %v", err)
	}
	want := "projects/tn__01m25db6arsvp2bvhbzfwa95se--"
	if prefix != want {
		t.Errorf("prefix = %q, want %q", prefix, want)
	}
}

func TestTenantSearchPrefixRejectsInvalidTenant(t *testing.T) {
	if _, err := tenantSearchPrefix(".."); err == nil {
		t.Error("a traversal-shaped tenant must not produce a prefix")
	}
}

func TestFilterTenantResultsKeepsOnlyOwnTenant(t *testing.T) {
	prefix, err := tenantSearchPrefix(testTenantID)
	if err != nil {
		t.Fatalf("tenantSearchPrefix: %v", err)
	}

	results := []duffel.SearchResult{
		{Path: "projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_a/memory.md"},
		{Path: "projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_b/lessons.md"},
		{Path: "/projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_c/lessons.md"},
		{Path: "projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_a/../outside.md"},
		{Path: "projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_a/.archive/deleted.md"},
		{Path: "projects/TN__01M25DB6ARSVP2BVHBZFWA95SE--mprj_a/memory.md"},
		{Path: "projects/tn__01othertenant--mprj_c/memory.md"},
		{Path: "projects/legacy-unscoped/memory.md"},
		{Path: "notes/random.md"},
	}

	filtered := filterTenantResults(results, prefix)
	if len(filtered) != 2 {
		t.Fatalf("filtered = %d results, want the two own-tenant hits: %+v", len(filtered), filtered)
	}
	for _, res := range filtered {
		if res.Path != "projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_a/memory.md" &&
			res.Path != "projects/tn__01m25db6arsvp2bvhbzfwa95se--mprj_b/lessons.md" {
			t.Errorf("foreign result survived filtering: %q", res.Path)
		}
	}
}
