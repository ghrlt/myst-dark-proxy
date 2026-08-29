package tunnel

import (
	"math/big"
	"testing"
)

func TestFormatMYST(t *testing.T) {
	cases := []struct {
		wei  string
		want string
	}{
		{"0", "0.0000"},
		// Dust below a ten-thousandth of a MYST reads as nothing, which is
		// what it is worth.
		{"1234", "0.0000"},
		{"10000000000000000", "0.0100"},
		{"1234500000000000000", "1.2345"},
		{"999900000000000000000", "999.9000"},
		// Well past what a uint64 of wei can hold: an always-on relay reaches
		// this in months, and the answer has to stay exact.
		{"123456789000000000000000000", "123456789.0000"},
		{"-1500000000000000000", "-1.5000"},
	}
	for _, tc := range cases {
		wei, ok := new(big.Int).SetString(tc.wei, 10)
		if !ok {
			t.Fatalf("bad test input %q", tc.wei)
		}
		if got := FormatMYST(wei); got != tc.want {
			t.Errorf("FormatMYST(%s) = %q, want %q", tc.wei, got, tc.want)
		}
	}
}

func TestFormatMYSTOfNothing(t *testing.T) {
	if got := FormatMYST(nil); got != "0.0000" {
		t.Errorf("FormatMYST(nil) = %q, want %q", got, "0.0000")
	}
}
