package config

import (
	"slices"
	"testing"
)

func TestNormaliseExitTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		// "cdn" is what a person types; content_delivery_network is what the
		// network calls it, and a policy spelled the first way must reach it.
		{"the short name for a CDN", []string{"cdn"}, []string{"content_delivery_network"}},
		{"punctuation and case", []string{" Content-Delivery-Network "}, []string{"content_delivery_network"}},
		{"a data centre by its other name", []string{"datacenter"}, []string{"hosting"}},
		{"several at once", []string{"residential", "CELLULAR"}, []string{"residential", "cellular"}},
		// "any" is the absence of a policy, so it cancels the list it is in
		// rather than being one more thing to look for.
		{"any", []string{"any"}, nil},
		{"any among others", []string{"residential", "any"}, nil},
		{"nothing at all", nil, nil},
		// The list in this build is its vocabulary, not the network's: a type
		// that appears on the network next month must still be usable today.
		{"a type this build has never heard of", []string{"quantum_relay"}, []string{"quantum_relay"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormaliseExitTypes(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExitTypeLabelKeepsWhatItDoesNotKnow(t *testing.T) {
	if got := ExitTypeLabel("content_delivery_network"); got != "Content delivery network" {
		t.Errorf("known type: got %q", got)
	}
	if got := ExitTypeLabel("quantum_relay"); got != "quantum_relay" {
		t.Errorf("unknown type: got %q, want it back unchanged", got)
	}
}
