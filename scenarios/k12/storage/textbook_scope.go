package k12storage

import (
	"fmt"
	"strings"
)

// TextbookScope keeps the trusted Knowledge principal separate from the K12
// tutor identity. OwnerID is supplied by server composition/authentication;
// AgentName remains an owner-internal profile dimension.
type TextbookScope struct {
	OwnerID   string
	AgentName string
	Subject   string
}

func (scope TextbookScope) normalized() (TextbookScope, error) {
	scope.OwnerID = strings.TrimSpace(scope.OwnerID)
	scope.AgentName = strings.TrimSpace(scope.AgentName)
	scope.Subject = strings.TrimSpace(scope.Subject)
	if scope.OwnerID == "" || scope.AgentName == "" || scope.Subject != textbookManifestSubjectMath {
		return TextbookScope{}, fmt.Errorf(
			"k12storage: owner, agent and subject=math required",
		)
	}
	return scope, nil
}
