package commands

// Phase 1 — pure functions, no network. These helpers are unexported, so the
// test lives in package commands. Signatures verified against the real source.

import "testing"

func TestWindowToMySQLInterval(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"24h", "24 HOUR", false},
		{"3d", "3 DAY", false},
		{"30m", "30 MINUTE", false},
		{"2w", "2 WEEK", false},
		{"45s", "45 SECOND", false},
		{"1H", "1 HOUR", false}, // uppercase unit accepted
		{"0h", "", true},        // n <= 0 rejected
		{"-2h", "", true},       // regex has no sign
		{"abc", "", true},       // no regex match
		{"10", "", true},        // missing unit
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := windowToMySQLInterval(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestSanitizeTableName pins W14's allowlist conversion: valid identifiers
// pass through unchanged, everything injection-shaped is REJECTED (an
// error), not silently stripped down to something that still runs.
func TestSanitizeTableName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"ordini", "ordini", false},
		{"pharmacy_product_listings", "pharmacy_product_listings", false},
		{"ordini; DROP TABLE x", "", true}, // ';' and space reject
		{`o'rdini`, "", true},              // single quote rejected
		{`o\rdini`, "", true},              // backslash rejected
		{`a';\b`, "", true},
		{"1ordini", "", true}, // leading digit rejected
		{"", "", true},
		{"ordini--comment", "", true}, // SQL comment marker rejected
	}
	for _, tc := range cases {
		got, err := sanitizeTableName(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("sanitizeTableName(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("sanitizeTableName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSanitizeInterval pins W14's allowlist conversion for --since values:
// valid INTERVAL-literal bodies pass through unchanged, injection-shaped
// input is REJECTED, not stripped.
func TestSanitizeInterval(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"24 hours", "24 hours", false},
		{"7 days", "7 days", false},
		{"1; DROP", "", true},           // ';' rejected
		{"30'min", "", true},            // quote rejected
		{"5\tdays\n", "", true},         // tab/newline rejected
		{"INTERVAL '7 days'", "", true}, // quote rejected
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := sanitizeInterval(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("sanitizeInterval(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("sanitizeInterval(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
