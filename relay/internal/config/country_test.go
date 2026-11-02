package config

import (
	"reflect"
	"testing"
)

func TestNormaliseCountries(t *testing.T) {
	got := NormaliseCountries([]string{" nl", "DE", "uk", "NL", ""})
	want := []string{"NL", "DE", "GB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCountryLabel(t *testing.T) {
	if got := CountryLabel("nl"); got != "Netherlands (NL)" {
		t.Fatalf("got %q", got)
	}
	if got := CountryLabel("zz"); got != "ZZ" {
		t.Fatalf("got %q", got)
	}
}
