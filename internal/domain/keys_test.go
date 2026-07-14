package domain

import (
	"reflect"
	"testing"
)

func TestMultiTabKeys(t *testing.T) {
	cases := []struct {
		name   string
		groups [][]string
		multi  []bool
		want   []string
	}{
		{
			name:   "all single-select: one digit per tab, no advance (auto-advances)",
			groups: [][]string{{"1"}, {"2"}, {"1"}},
			multi:  []bool{false, false, false},
			want:   []string{"1", "2", "1"},
		},
		{
			name:   "middle tab multi-select: toggles then explicit advance",
			groups: [][]string{{"1"}, {"1", "3"}, {"1"}},
			multi:  []bool{false, true, false},
			want:   []string{"1", "1", "3", "right", "1"},
		},
		{
			name:   "multi-select tab with a single chosen option STILL advances",
			groups: [][]string{{"1"}, {"2"}, {"1"}},
			multi:  []bool{false, true, false},
			want:   []string{"1", "2", "right", "1"},
		},
		{
			name:   "Submit tab (last) never advances even though earlier multi",
			groups: [][]string{{"1", "2"}, {"1"}},
			multi:  []bool{true, false},
			want:   []string{"1", "2", "right", "1"},
		},
		{
			name:   "nil multiSelect metadata degrades to single-select delivery",
			groups: [][]string{{"1"}, {"2"}},
			multi:  nil,
			want:   []string{"1", "2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MultiTabKeys(tc.groups, tc.multi, "right")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MultiTabKeys = %v, want %v", got, tc.want)
			}
		})
	}
}
