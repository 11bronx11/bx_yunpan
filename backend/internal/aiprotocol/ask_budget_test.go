package aiprotocol

import "testing"

func TestSelectedAskTierBoundaries(t *testing.T) {
	for _, tc := range []struct{ files, multiplier int }{
		{0, 1},
		{1, 5},
		{2, 4},
		{3, 4},
		{4, 3},
		{5, 3},
		{6, 2},
		{9, 2},
		{10, 1},
		{50, 1},
	} {
		if got := SelectedAskMultiplier(tc.files); got != tc.multiplier {
			t.Errorf("files=%d: multiplier=%d, want %d", tc.files, got, tc.multiplier)
		}
	}
}
