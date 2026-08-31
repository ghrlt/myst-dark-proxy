package systray

import "testing"

// Both desktops number a menu by walking it depth-first, and each does its own
// walk: Windows while building the popups, the panel's side while answering for
// them. What they have in common is this order, so it is pinned here rather
// than left to two implementations happening to agree.
func TestFlattenNumbersDepthFirst(t *testing.T) {
	menu := []Item{
		{Label: "status"},
		Submenu("exit",
			Item{Label: "residential"},
			Submenu("more", Item{Label: "cdn"}),
			Item{Label: "hosting"}),
		{Label: "quit"},
	}

	want := []string{"status", "exit", "residential", "more", "cdn", "hosting", "quit"}
	got := flatten(menu)
	if len(got) != len(want) {
		t.Fatalf("flattened %d items, want %d", len(got), len(want))
	}
	for i, label := range want {
		if got[i].Label != label {
			t.Errorf("id %d is %q, want %q", i+1, got[i].Label, label)
		}
	}
}
