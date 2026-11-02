package config

import "strings"

// Country is a place an exit can be in, by the ISO-3166 alpha-2 code the
// Mysterium network labels providers with.
type Country struct {
	Code string
	Name string
}

// Countries are the ones worth offering in a menu: where most of the network's
// providers are, plus the places people most often want to appear to be. The
// network is the authority, not this list - any other two-letter code still
// works by being spelled out, and is counted against the live providers before
// it is accepted.
var Countries = []Country{
	{"US", "United States"}, {"GB", "United Kingdom"}, {"DE", "Germany"}, {"FR", "France"},
	{"NL", "Netherlands"}, {"CA", "Canada"}, {"ES", "Spain"}, {"IT", "Italy"},
	{"CH", "Switzerland"}, {"SE", "Sweden"}, {"NO", "Norway"}, {"FI", "Finland"},
	{"DK", "Denmark"}, {"IE", "Ireland"}, {"BE", "Belgium"}, {"AT", "Austria"},
	{"PL", "Poland"}, {"CZ", "Czechia"}, {"PT", "Portugal"}, {"RO", "Romania"},
	{"UA", "Ukraine"}, {"TR", "Türkiye"}, {"IL", "Israel"}, {"AE", "United Arab Emirates"},
	{"IN", "India"}, {"JP", "Japan"}, {"KR", "South Korea"}, {"SG", "Singapore"},
	{"HK", "Hong Kong"}, {"AU", "Australia"}, {"BR", "Brazil"}, {"MX", "Mexico"},
	{"AR", "Argentina"}, {"ZA", "South Africa"},
}

// CountryLabel names a code the way a person would: "NL" as "Netherlands (NL)".
// A code this build does not list is returned as written.
func CountryLabel(code string) string {
	for _, c := range Countries {
		if strings.EqualFold(c.Code, code) {
			return c.Name + " (" + c.Code + ")"
		}
	}
	return strings.ToUpper(code)
}

// NormaliseCountries upper-cases, trims and de-duplicates country codes, and
// drops blanks. "UK" is accepted for GB because it is what people write.
func NormaliseCountries(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		v := strings.ToUpper(strings.TrimSpace(raw))
		if v == "UK" {
			v = "GB"
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
