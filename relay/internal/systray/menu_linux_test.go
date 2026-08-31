package systray

import "testing"

// The panel is handed a tree by GetLayout and hands back one id when something
// in it is clicked, and the two walks that produce those are different pieces
// of code. A menu numbered one way and read the other runs the wrong line -
// silently, and only for items below the first submenu - so the agreement is
// checked rather than assumed.
func TestLayoutNumbersTheSameWayFlattenDoes(t *testing.T) {
	menu := []Item{
		{Label: "status"},
		Submenu("exit",
			Item{Label: "residential", Checked: true},
			Item{Label: "cdn"}),
		{Label: "quit"},
	}

	next := int32(0)
	var ids []int32
	var labels []string
	var walk func(any)
	walk = func(n any) {
		fields := n.([]any)
		ids = append(ids, fields[0].(int32))
		props := fields[1].(map[string]variant)
		labels = append(labels, props["label"].V.(string))
		for _, child := range fields[2].([]any) {
			walk(child.(variant).V)
		}
	}
	for _, item := range menu {
		walk(node(item, &next))
	}

	flat := flatten(menu)
	if len(ids) != len(flat) {
		t.Fatalf("layout has %d items, flatten has %d", len(ids), len(flat))
	}
	for i := range ids {
		if int(ids[i]) != i+1 || labels[i] != flat[i].Label {
			t.Errorf("layout item %d is id %d %q, want id %d %q",
				i, ids[i], labels[i], i+1, flat[i].Label)
		}
	}
}

// A chosen alternative is marked, and marked as one of a group rather than as
// a tick a click could clear.
func TestACheckedItemIsARadioMark(t *testing.T) {
	props := menuItem(1, Item{Label: "residential", Checked: true}).([]any)[1].(map[string]variant)
	if got := props["toggle-type"].V; got != "radio" {
		t.Errorf("toggle-type: got %v, want radio", got)
	}
	if got := props["toggle-state"].V; got != int32(1) {
		t.Errorf("toggle-state: got %v, want 1", got)
	}
}

// An item that only opens a submenu has no action, and a panel that took
// "no action" for "not clickable" would refuse to open it.
func TestASubmenuIsEnabledWithoutAnActionOfItsOwn(t *testing.T) {
	props := menuItem(1, Submenu("exit", Item{Label: "residential"})).([]any)[1].(map[string]variant)
	if got := props["enabled"].V; got != true {
		t.Errorf("enabled: got %v, want true", got)
	}
	if got := props["children-display"].V; got != "submenu" {
		t.Errorf("children-display: got %v, want submenu", got)
	}
}
