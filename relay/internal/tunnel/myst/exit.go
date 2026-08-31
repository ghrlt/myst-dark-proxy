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

	matches, err := t.countExits(ctx, want)
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

// countExits asks the node how many providers the policy would leave to choose
// from. The node filters by one ip_type at a time, so a list is counted a type
// at a time; the filters are applied server-side, which keeps this a small
// answer rather than the whole proposal list.
func (t *Tunnel) countExits(ctx context.Context, types []string) (int, error) {
	policy := t.opt.Exit
	if policy.ProviderID != "" {
		// A pinned provider bypasses every filter, so there is nothing to count
		// and nothing the answer could change.
		return 1, nil
	}
	serviceType := policy.ServiceType
	if serviceType == "" {
		serviceType = "wireguard"
	}
	filter := tequilapi.ProposalFilter{ServiceType: serviceType, QualityMin: policy.MinQuality}
	if len(policy.Countries) == 1 {
		filter.Country = policy.Countries[0]
	}

	if len(types) == 0 {
		all, err := t.opt.Client.Proposals(ctx, filter)
		if err != nil {
			return 0, fmt.Errorf("ask the node what exits are available: %w", err)
		}
		// The configured types are what is being replaced here, not what is
		// being counted; leaving them in would count the policy on its way out.
		policy.IPTypes = nil
		return len(t.filter(all, policy)), nil
	}

	total := 0
	for _, ipType := range types {
		filter.IPType = ipType
		all, err := t.opt.Client.Proposals(ctx, filter)
		if err != nil {
			return 0, fmt.Errorf("ask the node what %s exits are available: %w", ipType, err)
		}
		// The server-side filter is a hint, not the policy: countries beyond
		// the first, excluded providers and cooldowns are applied here, so what
		// is counted is what pickProposal would actually have to choose from.
		narrowed := policy
		narrowed.IPTypes = []string{ipType}
		total += len(t.filter(all, narrowed))
	}
	return total, nil
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
