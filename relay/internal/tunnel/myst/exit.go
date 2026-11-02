package myst

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghrlt/myst-dark-proxy/relay/internal/config"
	"github.com/ghrlt/myst-dark-proxy/relay/internal/tequilapi"
)

// The kind of exit address - residential, cellular, hosting, a CDN - is the
// half of the exit policy worth changing while the relay is running. What it
// decides is how the exit looks to whatever is on the other end, and that is a
// judgement about the site being visited rather than about the relay: hosting
// exits are cheap and fast and are the first thing a site blocks, residential
// ones pass for a person at home and cost more.
//
// So EXIT_IP_TYPE sets what the relay starts with, and this is how that is
// moved afterwards, without a restart and without losing anything but the
// session being left behind.

// policy is the exit policy as it stands now: the configured one, with the
// live exit types in place of the configured ones.
func (t *Tunnel) policy() config.ExitPolicy {
	p := t.opt.Exit
	p.IPTypes = t.ExitTypes()
	p.Countries = t.ExitCountries()
	return p
}

// ExitTypes reports which kinds of exit address the policy currently allows.
// Empty is every kind.
func (t *Tunnel) ExitTypes() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]string(nil), t.ipTypes...)
}

// SetExitTypes narrows the exit policy to these kinds of address, or widens it
// to every kind when the list is empty. It returns the types as the node spells
// them and how many providers currently match them.
//
// The count is the point of asking the node rather than storing the answer and
// hoping. A type this build has never heard of may be perfectly real, and a
// type it knows well - government, education - may have five providers on the
// whole network tonight. Both look the same from here, and both are settings
// that would leave the relay unable to build a session at all. So a policy
// nothing matches is refused while there is still something to refuse it to,
// rather than accepted and discovered at the next client's expense.
func (t *Tunnel) SetExitTypes(ctx context.Context, types []string) ([]string, int, error) {
	want := config.NormaliseExitTypes(types)

	matches, err := t.countExits(ctx, t.ExitCountries(), want)
	if err != nil {
		return nil, 0, err
	}
	if matches == 0 {
		return nil, 0, fmt.Errorf("no provider on the network offers %s right now, so this relay would have nowhere to go",
			describeTypes(want))
	}

	t.mu.Lock()
	t.ipTypes = want
	t.mu.Unlock()

	t.log.Info("exit type policy changed", "ip_types", describeTypes(want), "providers", matches)
	return want, matches, nil
}

// ExitCountries reports which countries the policy currently allows, as
// ISO-3166 alpha-2 codes. Empty is every country.
func (t *Tunnel) ExitCountries() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]string(nil), t.countries...)
}

// SetExitCountries narrows the exit policy to these countries, or widens it to
// every country when the list is empty. Like SetExitTypes it returns the codes
// as the node spells them and how many providers match, and refuses a policy
// nothing matches: plenty of countries have no provider tonight, and the relay
// must not be left with nowhere to go.
func (t *Tunnel) SetExitCountries(ctx context.Context, countries []string) ([]string, int, error) {
	want := config.NormaliseCountries(countries)

	matches, err := t.countExits(ctx, want, t.ExitTypes())
	if err != nil {
		return nil, 0, err
	}
	if matches == 0 {
		return nil, 0, fmt.Errorf("no provider in %s offers a matching exit right now, so this relay would have nowhere to go",
			describeCountries(want))
	}

	t.mu.Lock()
	t.countries = want
	t.mu.Unlock()

	t.log.Info("exit country policy changed", "countries", describeCountries(want), "providers", matches)
	return want, matches, nil
}

// countExits asks the node how many providers a policy of these countries and
// kinds would leave to choose from. The node filters by one country and one
// ip_type at a time, so when there are several the rest is left to t.filter,
// which is also where excluded providers and cooldowns are applied - so what is
// counted is what pickProposal would actually have to choose from.
func (t *Tunnel) countExits(ctx context.Context, countries, types []string) (int, error) {
	policy := t.opt.Exit
	if policy.ProviderID != "" {
		// A pinned provider bypasses every filter, so there is nothing to count
		// and nothing the answer could change.
		return 1, nil
	}
	policy.Countries = countries
	policy.IPTypes = types

	serviceType := policy.ServiceType
	if serviceType == "" {
		serviceType = "wireguard"
	}
	filter := tequilapi.ProposalFilter{ServiceType: serviceType, QualityMin: policy.MinQuality}
	if len(countries) == 1 {
		filter.Country = countries[0]
	}
	if len(types) == 1 {
		filter.IPType = types[0]
	}
	all, err := t.opt.Client.Proposals(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("ask the node what exits are available: %w", err)
	}
	return len(t.filter(all, policy)), nil
}

// describeTypes names a policy the way the message using it needs to read.
func describeTypes(types []string) string {
	if len(types) == 0 {
		return "any exit type"
	}
	labels := make([]string, 0, len(types))
	for _, t := range types {
		labels = append(labels, config.ExitTypeLabel(t))
	}
	return strings.Join(labels, ", ")
}

func describeCountries(countries []string) string {
	if len(countries) == 0 {
		return "any country"
	}
	labels := make([]string, 0, len(countries))
	for _, c := range countries {
		labels = append(labels, config.CountryLabel(c))
	}
	return strings.Join(labels, ", ")
}
