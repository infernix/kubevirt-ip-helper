package util

import "testing"

func TestAllocationRefRoundTrip(t *testing.T) {
	const ref = "default/vm-test [02:00:00:00:00:01]"

	if got := AllocationRef("default", "vm-test", "02:00:00:00:00:01"); got != ref {
		t.Errorf("AllocationRef = %q, want %q", got, ref)
	}

	// a non-canonical but parseable mac spelling normalizes to the
	// canonical colon form
	if got := AllocationRef("default", "vm-test", "02-00-00-00-00-01"); got != ref {
		t.Errorf("AllocationRef with a non-canonical mac = %q, want %q", got, ref)
	}

	ns, vm, hw, ok := ParseAllocationRef(ref)
	if !ok {
		t.Fatal("ParseAllocationRef rejected the canonical reference")
	}
	if ns != "default" || vm != "vm-test" || hw != "02:00:00:00:00:01" {
		t.Errorf("ParseAllocationRef = %q/%q/%q, want default/vm-test/02:00:00:00:00:01", ns, vm, hw)
	}
}

func TestParseAllocationRefRejectsGarbage(t *testing.T) {
	for _, ref := range []string{
		"",
		"EXCLUDED",
		"USED",
		"no-slash",
		"vmname [02:00:00:00:00:01]",
		"default/vm-1",
		"/missing-namespace [02:00:00:00:00:01]",
		"default/ [02:00:00:00:00:01]",
		"default/vm []",
		"default/vm [not a mac!]",
		"default/vm [02:00:00:00:00:01",
		"bracket-open-only [abc",
		// exactly one owner separator
		"default/extra/owner [02:00:00:00:00:01]",
		// no brackets or whitespace inside the owner
		"default/[vm] [02:00:00:00:00:01]",
		"default/vm name [02:00:00:00:00:01]",
		"default/vm\tname [02:00:00:00:00:01]",
		"default/vm\u00a0name [02:00:00:00:00:01]",
		"default/vm\u2003name [02:00:00:00:00:01]",
		"default/vm\nname [02:00:00:00:00:01]",
		// a trailing tail after the closing bracket
		"default/vm [02:00:00:00:00:01] extra",
	} {
		if _, _, _, ok := ParseAllocationRef(ref); ok {
			t.Errorf("ParseAllocationRef(%q) = ok, want rejection", ref)
		}
	}
}

func TestParseAllocationRefCanonicalizesTheMac(t *testing.T) {
	// the parse output itself is canonical whatever spelling the
	// reference carries: a future consumer which compares the returned
	// hardware address verbatim must never split one logical owner in
	// two (net.ParseMAC accepts the dash and uppercase spellings of
	// older revisions and hand-edited status)
	cases := []struct {
		ref    string
		wantNS string
		want   string
	}{
		{"default/vm-test [02-00-00-00-00-01]", "default", "02:00:00:00:00:01"},
		{"default/vm-test [02:00:00:00:00:01]", "default", "02:00:00:00:00:01"},
		{"default/vm-test [02:AA:BB:CC:DD:01]", "default", "02:aa:bb:cc:dd:01"},
		// the owner text is not normalized: an uppercase owner of an
		// older revision keeps its spelling
		{"DEFAULT/vm-test [02:AA:BB:CC:DD:01]", "DEFAULT", "02:aa:bb:cc:dd:01"},
	}
	for _, tc := range cases {
		ns, vm, hw, ok := ParseAllocationRef(tc.ref)
		if !ok {
			t.Errorf("ParseAllocationRef(%q) rejected a parseable legacy spelling", tc.ref)
			continue
		}
		if ns != tc.wantNS || vm != "vm-test" || hw != tc.want {
			t.Errorf("ParseAllocationRef(%q) = %q/%q/%q, want %s/vm-test/%s", tc.ref, ns, vm, hw, tc.wantNS, tc.want)
		}
	}
}
