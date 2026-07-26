package buildinfo

import "testing"

func TestLabelOf(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare semver gets a v", "0.5.2", "v0.5.2"},
		{"tag name already prefixed", "v0.5.2", "v0.5.2"},
		{"unstamped default stays verbatim", "dev", "dev"},
		{"makefile dev build stays verbatim", "dev-20260726120000", "dev-20260726120000"},
		{"surrounding space trimmed", " 1.0.0 ", "v1.0.0"},
		{"empty stays empty", "", ""},
		{"blank stays empty", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelOf(tc.in); got != tc.want {
				t.Errorf("LabelOf(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLabelUsesVersion keeps Label wired to the stamped package var.
func TestLabelUsesVersion(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "0.5.2"
	if got := Label(); got != "v0.5.2" {
		t.Errorf("Label() = %q, want %q", got, "v0.5.2")
	}
}
