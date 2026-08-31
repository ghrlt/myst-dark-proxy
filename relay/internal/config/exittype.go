package config

import "strings"

// ExitType is one of the kinds of address the Mysterium network labels a
// provider's egress with. It is the setting that decides what the exit looks
// like to whatever is on the other end: a residential address passes for a
// person at home, a hosting one is visibly a data centre and is what most
// sites that block anything block first.
//
// The network is the authority on the values, not this list. It is here so the
// menu has something to offer and so the names people actually use - "cdn",
// "datacenter" - reach the value the node knows, and an exit type that appears
// on the network tomorrow still works today by being spelled out in full.
type ExitType struct {
	// Value is what the node calls it, and what EXIT_IP_TYPE takes.
	Value string
	// Label is what a person calls it.
	Label string
	// Aliases are the other spellings accepted for Value.
	Aliases []string
}

// ExitTypes are the kinds worth offering, commonest first. The three left out -
// organization, search_engine_spider, and whatever the network adds next - are
// a handful of providers between them and are still reachable by naming them.
var ExitTypes = []ExitType{
	{Value: "residential", Label: "Residential"},
	{Value: "cellular", Label: "Cellular"},
	{Value: "business", Label: "Business"},
	{Value: "hosting", Label: "Hosting (data centre)", Aliases: []string{"datacenter", "datacentre", "data_center"}},
	{Value: "content_delivery_network", Label: "Content delivery network", Aliases: []string{"cdn"}},
	{Value: "education", Label: "Education"},
	{Value: "government", Label: "Government"},
}

// ExitTypeLabel names a value the way a person would. An unknown one is
// returned as it was written: the network may well know a type this build does
// not, and inventing a label for it would be a worse answer than showing it.
func ExitTypeLabel(value string) string {
	for _, t := range ExitTypes {
		if strings.EqualFold(t.Value, value) {
			return t.Label
		}
	}
	return value
}

// NormaliseExitTypes turns what somebody wrote into what the node understands.
//
// Values it does not recognise are passed through rather than refused. The list
// above is this build's vocabulary, not the network's, and a policy naming a
// type that has no providers is caught where it can be answered honestly - by
// counting what actually matches it - rather than here, where all that could be
// said is that this build has not heard of it.
func NormaliseExitTypes(in []string) []string {
	var out []string
	for _, raw := range in {
		v := strings.ToLower(strings.TrimSpace(raw))
		v = strings.ReplaceAll(strings.ReplaceAll(v, "-", "_"), " ", "_")
		switch v {
		case "", "any", "all":
			// "any type" is the absence of a constraint, not a constraint
			// nothing satisfies. It cancels the rest of the list with it.
			return nil
		}
		out = append(out, canonicalExitType(v))
	}
	return out
}

func canonicalExitType(v string) string {
	for _, t := range ExitTypes {
		if t.Value == v {
			return t.Value
		}
		for _, a := range t.Aliases {
			if a == v {
				return t.Value
			}
		}
	}
	return v
}
