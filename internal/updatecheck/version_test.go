package updatecheck

import "testing"

func TestIsRelease(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"v0.5.2", true},
		{"0.5.2", true},
		{"1.0", true},
		{"2", true},
		{"v1.2.3-rc1", true},
		{"dev", false},
		{"dev-20260726120000", false},
		{"", false},
		{"   ", false},
		{"v", false},
		{"1.2.3.4", false},
		{"1.x.3", false},
		{"-1.0.0", false},
	}
	for _, tc := range cases {
		if got := IsRelease(tc.in); got != tc.want {
			t.Errorf("IsRelease(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.5.2", "v0.5.2", 0},
		{"0.5.2", "v0.5.2", 0}, // prefix is cosmetic
		{"v0.5.1", "v0.5.2", -1},
		{"v0.5.2", "v0.5.1", 1},
		{"v0.5.9", "v0.6.0", -1},
		{"v0.9.0", "v1.0.0", -1},
		{"v1.0.0", "v0.9.9", 1},
		{"v1.2.3-rc1", "v1.2.3", 0}, // suffix ignored
		{"v1.2", "v1.2.0", 0},       // missing components are zero
		{"dev", "dev", 0},           // two unparseable values never order
		{"dev", "v0.1.0", -1},       // a non-release sorts below a release
		{"v0.1.0", "dev", 1},
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestIsNewer is the operator-visible contract: only a real release upgrade
// may light the header hint.
func TestIsNewer(t *testing.T) {
	cases := []struct {
		name            string
		current, latest string
		want            bool
	}{
		{"patch upgrade", "v0.5.1", "v0.5.2", true},
		{"minor upgrade", "v0.5.9", "v0.6.0", true},
		{"same version", "v0.5.2", "v0.5.2", false},
		{"downgrade", "v0.5.2", "v0.5.1", false},
		{"prefix mismatch is not a change", "0.5.2", "v0.5.2", false},
		{"dev build never nags", "dev", "v9.9.9", false},
		{"makefile dev build never nags", "dev-20260726120000", "v9.9.9", false},
		{"unstamped never nags", "", "v9.9.9", false},
		{"garbage latest is ignored", "v0.5.1", "not-a-version", false},
		{"empty latest is ignored", "v0.5.1", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNewer(tc.current, tc.latest); got != tc.want {
				t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}
