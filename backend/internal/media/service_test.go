package media

import "testing"

func TestValidSourceDimensions(t *testing.T) {
	for _, test := range []struct {
		width, height int
		valid         bool
	}{
		{width: 6000, height: 6000, valid: true},
		{width: 10000, height: 5000, valid: false},
		{width: 0, height: 100, valid: false},
	} {
		if actual := validSourceDimensions(test.width, test.height); actual != test.valid {
			t.Fatalf("dimensions %dx%d: expected %t, got %t", test.width, test.height, test.valid, actual)
		}
	}
}
