package usecase

import (
	"context"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// creationGradeTerm resolves the immutable attribution written into a newly
// created learning object. An explicit, valid request grade wins; otherwise the
// current persisted profile is read once. Failure stays empty rather than
// inventing a term.
func (d Deps) creationGradeTerm(ctx context.Context, agentName, explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" && k12.ValidProfileGradeTerm(explicit) {
		return explicit
	}
	if d.Profiles != nil {
		if profile, err := d.GetProfile(ctx, agentName); err == nil &&
			k12.ValidProfileGradeTerm(profile.GradeTerm) {
			return profile.GradeTerm
		}
	}
	if d.Records != nil {
		if grade, err := d.Records.AgentGradeTerm(ctx, agentName); err == nil &&
			k12.ValidProfileGradeTerm(grade) {
			return grade
		}
	}
	return ""
}
