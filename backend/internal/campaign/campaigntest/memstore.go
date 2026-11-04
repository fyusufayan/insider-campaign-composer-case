// Package campaigntest provides test doubles for the campaign service so that
// the service and the HTTP layer can be unit-tested without PostgreSQL. The
// PostgreSQL repository itself is covered by the integration tests. Only test
// code imports this package.
package campaigntest

import (
	"context"
	"crypto/rand"
	"sync"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// MemoryStore is an in-memory campaign.Store with the same semantics as the
// repository: Update serializes per store (like the campaign row lock),
// applies the mutation to a copy and writes nothing when it fails or reports
// no change.
type MemoryStore struct {
	mu        sync.Mutex
	campaigns map[campaign.ID]campaign.Campaign
	// Updates counts committed writes (creates excluded).
	updates int
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{campaigns: map[campaign.ID]campaign.Campaign{}}
}

// Create implements campaign.Store.
func (m *MemoryStore) Create(_ context.Context, d campaign.Draft) (campaign.Campaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var id campaign.ID
	_, _ = rand.Read(id[:])
	now := time.Now().UTC()
	c := campaign.Campaign{
		ID: id, Name: d.Name, Brief: d.Brief, Status: campaign.StatusDraft,
		Channels: []segment.Channel{}, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	m.campaigns[id] = c
	return copyOf(c), nil
}

// Get implements campaign.Store.
func (m *MemoryStore) Get(_ context.Context, id campaign.ID) (campaign.Campaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.campaigns[id]
	if !ok {
		return campaign.Campaign{}, campaign.ErrNotFound
	}
	return copyOf(c), nil
}

// Update implements campaign.Store.
func (m *MemoryStore) Update(ctx context.Context, id campaign.ID, mutate campaign.Mutation) (campaign.Campaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return campaign.Campaign{}, err
	}
	cur, ok := m.campaigns[id]
	if !ok {
		return campaign.Campaign{}, campaign.ErrNotFound
	}
	next, changed, err := mutate(copyOf(cur))
	if err != nil {
		return campaign.Campaign{}, err
	}
	if !changed {
		return copyOf(cur), nil
	}
	next.UpdatedAt = time.Now().UTC()
	m.campaigns[id] = copyOf(next)
	m.updates++
	return copyOf(next), nil
}

// Put stores c as is (test setup, e.g. a status that Slice 1a cannot reach).
func (m *MemoryStore) Put(c campaign.Campaign) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.campaigns[c.ID] = copyOf(c)
}

// Updates returns the number of committed updates.
func (m *MemoryStore) Updates() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updates
}

// copyOf deep-copies the mutable parts of a campaign.
func copyOf(c campaign.Campaign) campaign.Campaign {
	c.Channels = append([]segment.Channel{}, c.Channels...)
	if c.Interpretation != nil {
		rec := *c.Interpretation
		rec.Assumptions = append([]string{}, rec.Assumptions...)
		rec.Unsupported = append([]string{}, rec.Unsupported...)
		c.Interpretation = &rec
	}
	if c.ContentBrief != nil {
		c.ContentBrief = append([]byte(nil), c.ContentBrief...)
	}
	return c
}

// Audience is a campaign.AudienceCounter double.
type Audience struct {
	// Func computes the result; when nil, Stats returns a fixed partition of
	// 10 matched users (the same per channel) for the selected channels.
	Func func(ctx context.Context, rule segment.Validated, channels []segment.Channel) (segment.AudienceStats, error)

	mu    sync.Mutex
	calls int
}

// Stats implements campaign.AudienceCounter.
func (a *Audience) Stats(ctx context.Context, rule segment.Validated, channels []segment.Channel) (segment.AudienceStats, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.Func != nil {
		return a.Func(ctx, rule, channels)
	}
	return FixedStats(channels), nil
}

// Calls returns the number of Stats calls.
func (a *Audience) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// FixedStats is the default partition: 10 matched users; per selected
// channel 6 eligible, 3 no_consent, 1 no_contact; reach consistent with the
// selection.
func FixedStats(channels []segment.Channel) segment.AudienceStats {
	sel, _ := segment.NormalizeChannels(channels)
	s := segment.AudienceStats{
		MatchedUsers: 10,
		ByChannel:    map[segment.Channel]segment.ChannelStats{},
		ComputedAt:   time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
	}
	for _, ch := range sel {
		s.ByChannel[ch] = segment.ChannelStats{Eligible: 6, NoConsent: 3, NoContact: 1}
		s.Messages += 6
	}
	switch len(sel) {
	case 2:
		s.ReachUsers = segment.ReachStats{Both: 6, Unreachable: 4}
	case 1:
		if sel[0] == segment.ChannelSMS {
			s.ReachUsers = segment.ReachStats{SMSOnly: 6, Unreachable: 4}
		} else {
			s.ReachUsers = segment.ReachStats{EmailOnly: 6, Unreachable: 4}
		}
	default:
		s.ReachUsers = segment.ReachStats{Unreachable: 10}
	}
	return s
}
