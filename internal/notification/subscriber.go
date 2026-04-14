package notification

import (
	"context"
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// notifiableTypes are the event types that can trigger notifications.
var notifiableTypes = []event.Type{
	event.TypeTaskFailed,
	event.TypeRunFailed,
	event.TypeRunTimedOut,
	event.TypeSLAMissed,
	event.TypeRunCompleted,
	event.TypeTaskSucceeded,
}

// Subscriber listens to the event bus and dispatches notifications
// through matching policies and channels.
type Subscriber struct {
	bus      event.Bus
	db       *gorm.DB
	senders  map[models.ChannelType]Sender
}

// NewSubscriber creates a notification subscriber.
func NewSubscriber(bus event.Bus, db *gorm.DB) *Subscriber {
	return &Subscriber{
		bus:     bus,
		db:      db,
		senders: make(map[models.ChannelType]Sender),
	}
}

// RegisterSender registers a Sender for a given channel type.
func (s *Subscriber) RegisterSender(ct models.ChannelType, sender Sender) {
	s.senders[ct] = sender
}

// Start subscribes to notifiable events and dispatches notifications.
func (s *Subscriber) Start(ctx context.Context) error {
	return s.StartWithReady(ctx, nil)
}

// StartWithReady subscribes and signals readiness after subscription is established.
func (s *Subscriber) StartWithReady(ctx context.Context, ready chan<- struct{}) error {
	filter := event.Filter{
		Types: notifiableTypes,
	}

	ch, err := s.bus.Subscribe(ctx, filter)
	if err != nil {
		return err
	}
	if ready != nil {
		close(ready)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-ch:
			if !ok {
				return nil
			}
			s.handleEvent(ctx, evt)
		}
	}
}

func (s *Subscriber) handleEvent(ctx context.Context, evt event.Event) {
	// Record failure/alert metrics.
	recordEventMetric(evt)

	policies, err := s.matchPolicies(ctx, evt)
	if err != nil {
		log.Error("notification: failed to load policies",
			"event_type", string(evt.Type),
			"error", err,
		)
		return
	}

	if len(policies) == 0 {
		return
	}

	// Batch-load all referenced channels in a single query.
	channels, err := s.loadChannels(ctx, policies)
	if err != nil {
		log.Error("notification: failed to load channels",
			"error", err,
		)
		return
	}

	payload := buildPayload(evt)

	for _, policy := range policies {
		channel, ok := channels[policy.ChannelID]
		if !ok {
			log.Error("notification: channel not found",
				"channel_id", policy.ChannelID,
			)
			continue
		}

		if !channel.Enabled {
			continue
		}

		sender, ok := s.senders[channel.Type]
		if !ok {
			log.Warn("notification: no sender registered",
				"channel_type", string(channel.Type),
				"channel_name", channel.Name,
			)
			continue
		}

		start := time.Now()
		sendErr := sender.Send(ctx, channel, payload)
		duration := time.Since(start)

		NotificationSendDuration.WithLabelValues(string(channel.Type)).Observe(duration.Seconds())

		if sendErr != nil {
			NotificationSendsTotal.WithLabelValues(string(channel.Type), "error").Inc()
			log.Error("notification: send failed",
				"channel_name", channel.Name,
				"channel_type", string(channel.Type),
				"event_type", string(evt.Type),
				"error", sendErr,
			)
		} else {
			NotificationSendsTotal.WithLabelValues(string(channel.Type), "success").Inc()
		}
	}
}

// loadChannels fetches all unique channels referenced by the given policies
// in a single query, returning them indexed by ID.
func (s *Subscriber) loadChannels(ctx context.Context, policies []models.NotificationPolicy) (map[uuid.UUID]models.NotificationChannel, error) {
	ids := make([]uuid.UUID, 0, len(policies))
	seen := make(map[uuid.UUID]struct{}, len(policies))
	for _, p := range policies {
		if _, dup := seen[p.ChannelID]; !dup {
			ids = append(ids, p.ChannelID)
			seen[p.ChannelID] = struct{}{}
		}
	}

	var channels []models.NotificationChannel
	if err := s.db.WithContext(ctx).
		Where("id IN ?", ids).
		Find(&channels).Error; err != nil {
		return nil, err
	}

	m := make(map[uuid.UUID]models.NotificationChannel, len(channels))
	for _, ch := range channels {
		m[ch.ID] = ch
	}
	return m, nil
}

// matchPolicies finds enabled policies whose event_types contain the given
// event type and whose filters match the event. Uses a SQL-level filter on
// event type to reduce the rows loaded from the database.
func (s *Subscriber) matchPolicies(ctx context.Context, evt event.Event) ([]models.NotificationPolicy, error) {
	var candidates []models.NotificationPolicy
	// Filter at the SQL level: only load policies whose event_types JSON
	// contains the event type string. This is a substring match on the
	// JSON column — not exact, but it eliminates the vast majority of
	// non-matching rows. The in-memory policyMatchesEvent check below
	// is the authoritative filter.
	if err := s.db.WithContext(ctx).
		Where("enabled = ? AND event_types LIKE ?", true, "%"+string(evt.Type)+"%").
		Find(&candidates).Error; err != nil {
		return nil, err
	}

	var matched []models.NotificationPolicy
	for _, p := range candidates {
		if !policyMatchesEvent(p, evt) {
			continue
		}
		if !policyFilterMatches(p, evt) {
			continue
		}
		matched = append(matched, p)
	}
	return matched, nil
}

// policyMatchesEvent checks whether the policy's event_types list contains evt.Type.
func policyMatchesEvent(p models.NotificationPolicy, evt event.Event) bool {
	var eventTypes []string
	if err := json.Unmarshal(p.EventTypes, &eventTypes); err != nil {
		return false
	}
	for _, t := range eventTypes {
		if event.Type(t) == evt.Type {
			return true
		}
	}
	return false
}

// policyFilterMatches checks optional job-scoped filters. Fails closed:
// returns false when the filter JSON is malformed or when a filter field
// cannot be evaluated from the event payload.
func policyFilterMatches(p models.NotificationPolicy, evt event.Event) bool {
	if len(p.Filters) == 0 {
		return true
	}

	var filter PolicyFilter
	if err := json.Unmarshal(p.Filters, &filter); err != nil {
		log.Warn("notification: invalid policy filter JSON, skipping (fail closed)",
			"policy_id", p.ID,
			"error", err,
		)
		return false
	}

	if len(filter.JobIDs) > 0 {
		found := false
		for _, id := range filter.JobIDs {
			if id == evt.JobID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if filter.JobAlias != "" {
		// If the payload is missing or unparseable, fail closed: the
		// filter requires a specific alias but we can't verify it.
		if len(evt.Payload) == 0 {
			return false
		}
		var partial struct {
			JobAlias string `json:"job_alias"`
		}
		if err := json.Unmarshal(evt.Payload, &partial); err != nil {
			return false
		}
		if partial.JobAlias != filter.JobAlias {
			return false
		}
	}

	return true
}

func buildPayload(evt event.Event) Payload {
	p := Payload{
		EventType:  evt.Type,
		JobID:      evt.JobID,
		RunID:      evt.RunID,
		TaskID:     evt.TaskID,
		Timestamp:  evt.Timestamp,
		RawPayload: evt.Payload,
	}

	// Extract common fields from the event payload.
	if evt.Payload != nil {
		var partial struct {
			JobAlias string `json:"job_alias"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(evt.Payload, &partial); err == nil {
			p.JobAlias = partial.JobAlias
			p.Error = partial.Error
		}
	}

	return p
}

// DecodePolicyEventTypes parses the JSON event types from a policy.
func DecodePolicyEventTypes(raw json.RawMessage) ([]event.Type, error) {
	var types []string
	if err := json.Unmarshal(raw, &types); err != nil {
		return nil, err
	}
	result := make([]event.Type, len(types))
	for i, t := range types {
		result[i] = event.Type(t)
	}
	return result, nil
}

// ValidEventTypes returns the set of event types valid for notification policies.
func ValidEventTypes() map[event.Type]struct{} {
	m := make(map[event.Type]struct{}, len(notifiableTypes))
	for _, t := range notifiableTypes {
		m[t] = struct{}{}
	}
	return m
}

// ValidChannelTypes returns the set of valid channel types.
func ValidChannelTypes() map[models.ChannelType]struct{} {
	return map[models.ChannelType]struct{}{
		models.ChannelTypeWebhook:   {},
		models.ChannelTypeSlack:     {},
		models.ChannelTypeEmail:     {},
		models.ChannelTypePagerDuty: {},
		models.ChannelTypeAIAgent:   {},
	}
}

// recordEventMetric increments the appropriate failure/alert counter.
func recordEventMetric(evt event.Event) {
	alias := extractJobAlias(evt)
	switch evt.Type {
	case event.TypeTaskFailed:
		TaskFailuresTotal.WithLabelValues(alias).Inc()
	case event.TypeRunFailed:
		RunFailuresTotal.WithLabelValues(alias).Inc()
	case event.TypeRunTimedOut:
		RunTimeoutsTotal.WithLabelValues(alias).Inc()
	case event.TypeSLAMissed:
		SLAMissesTotal.WithLabelValues(alias).Inc()
	}
}

func extractJobAlias(evt event.Event) string {
	if len(evt.Payload) == 0 {
		return ""
	}
	var partial struct {
		JobAlias string `json:"job_alias"`
	}
	if err := json.Unmarshal(evt.Payload, &partial); err == nil {
		return partial.JobAlias
	}
	return ""
}

// ChannelConfigMap extracts the channel's config as a raw map.
func ChannelConfigMap(ch models.NotificationChannel) (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(ch.Config, &m); err != nil {
		return nil, err
	}
	return m, nil
}

