package office_copilot

import (
	"encoding/json"
	"time"
)

// QuotaSnapshot contains only allowlisted usage metadata, never chat content.
// Missing fields mean unknown, not zero or unlimited. Totals and reset times
// are deliberately absent: the observed protocol does not supply them.
type QuotaSnapshot struct {
	ObservedAt        time.Time             `json:"observed_at"`
	Model             string                `json:"model"`
	ConversationUsed  *int                  `json:"conversation_used,omitempty"`
	ConversationLimit *int                  `json:"conversation_limit,omitempty"`
	Meters            map[string]QuotaMeter `json:"meters,omitempty"`
	Restrictions      []MeteringRestriction `json:"restrictions,omitempty"`
}

type QuotaMeter struct {
	Remaining *int `json:"remainingAllowance,omitempty"`
}

type chatHubThrottling struct {
	ConversationUsed  *int                       `json:"numUserMessagesInConversation"`
	ConversationLimit *int                       `json:"maxNumUserMessagesInConversation"`
	Metering          map[string]json.RawMessage `json:"metering"`
}

// These keys occur in the official Word Opus throttling UI. That UI is gated
// by AAD account type and feature flags, which these observations do not prove.
// Preserve them as diagnostics, not a verified account balance. GPT meter zeros
// must never be interpreted as exhausted Claude quota.
func knownClaudeMeter(key string) bool {
	switch key {
	case "ClaudeOpusQuery75", "ClaudeOpusQueryWeeklyWord", "ClaudeOpusQueryDailyWord", "ClaudeOpusQueryDaily", "ClaudeOpusQueryDev":
		return true
	}
	return false
}

func (q *QuotaSnapshot) observe(raw json.RawMessage) {
	var t *chatHubThrottling
	// Optional quota metadata must not cause an otherwise valid chat to fail.
	if json.Unmarshal(raw, &t) != nil || t == nil {
		return
	}
	observed := false
	if t.ConversationUsed != nil && *t.ConversationUsed >= 0 {
		q.ConversationUsed = t.ConversationUsed
		observed = true
	}
	if t.ConversationLimit != nil && *t.ConversationLimit > 0 {
		q.ConversationLimit = t.ConversationLimit
		observed = true
	}
	if t.Metering != nil {
		// Replace, rather than carry forward counters omitted by a newer update.
		q.Meters = make(map[string]QuotaMeter)
		for key, rawMeter := range t.Metering {
			var meter QuotaMeter
			if knownClaudeMeter(key) && json.Unmarshal(rawMeter, &meter) == nil && meter.Remaining != nil && *meter.Remaining >= 0 {
				q.Meters[key] = meter
				observed = true
			}
		}
	}
	if observed {
		q.ObservedAt = time.Now().UTC()
	}
}

// HasObservation excludes absent, malformed and unrelated counters. A failed
// stream without metadata must not overwrite a previously useful snapshot.
func (q QuotaSnapshot) HasObservation() bool {
	return !q.ObservedAt.IsZero() && (q.ConversationUsed != nil || q.ConversationLimit != nil || len(q.Meters) > 0 || len(q.Restrictions) > 0)
}

// Only normalized codes are persisted; raw upstream errors may contain private
// content. Scope unknown must not be interpreted as an account-wide block.
type MeteringRestriction struct {
	Code      string `json:"code"`
	Category  string `json:"category"`
	Scope     string `json:"scope"`
	HasAccess *bool  `json:"has_access,omitempty"`
}

func (q *QuotaSnapshot) observeMetering(raw json.RawMessage) {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return
	}
	for _, entry := range entries {
		var meter struct {
			Error  string `json:"meterError"`
			Access *bool  `json:"hasAccess"`
		}
		if json.Unmarshal(entry, &meter) != nil {
			continue
		}
		if meter.Error == "" && (meter.Access == nil || *meter.Access) {
			continue
		}
		r := MeteringRestriction{Code: "UpstreamMeteringRestriction", Category: "access", Scope: "unknown", HasAccess: meter.Access}
		switch meter.Error {
		case "ImageGenInsufficientTokensThrottled":
			r.Code, r.Category, r.Scope = meter.Error, "quota", "image_generation"
		case "ImageGenSystemCapacityThrottled":
			r.Code, r.Category, r.Scope = meter.Error, "capacity", "image_generation"
		}
		duplicate := false
		for _, old := range q.Restrictions {
			if old.Code == r.Code && old.Scope == r.Scope {
				duplicate = true
				break
			}
		}
		if !duplicate && len(q.Restrictions) < 16 {
			q.Restrictions = append(q.Restrictions, r)
		}
		q.ObservedAt = time.Now().UTC()
	}
}

// MeteringError is returned only when inference fails or produces no answer.
// A successful text answer may still carry a denied optional image capability.
type MeteringError struct{ Restriction MeteringRestriction }

func (e *MeteringError) Error() string {
	switch e.Restriction.Code {
	case "ImageGenInsufficientTokensThrottled":
		return "Office Copilot image generation quota is exhausted; this does not establish the text model's remaining quota"
	case "ImageGenSystemCapacityThrottled":
		return "Office Copilot image generation is temporarily limited by upstream capacity"
	default:
		return "Office Copilot reported an upstream capability access restriction; remaining account quota is unknown"
	}
}

func (q QuotaSnapshot) RestrictionError() error {
	if len(q.Restrictions) == 0 {
		return nil
	}
	return &MeteringError{Restriction: q.Restrictions[0]}
}
