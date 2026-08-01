package apihttp

import (
	"context"
	"errors"
	"strings"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func (h *handler) ownerScope(ctx context.Context) (string, error) {
	switch strings.TrimSpace(h.rt.PrincipalMode) {
	case "local_loopback":
		ownerID := strings.TrimSpace(h.rt.OwnerScope)
		if ownerID == "" {
			return "", errors.New("local owner scope missing")
		}
		return ownerID, nil
	case "remote":
		if h.rt.AuthenticatedOwnerScope == nil {
			return "", errors.New("remote principal resolver missing")
		}
		ownerID, err := h.rt.AuthenticatedOwnerScope(ctx)
		if err != nil || strings.TrimSpace(ownerID) == "" {
			return "", errors.New("authenticated owner scope missing")
		}
		return strings.TrimSpace(ownerID), nil
	default:
		return "", errors.New("unsupported principal mode")
	}
}

func (h *handler) textbookScope(
	ctx context.Context,
	agentName, subject string,
) (k12storage.TextbookScope, error) {
	ownerID, err := h.ownerScope(ctx)
	if err != nil {
		return k12storage.TextbookScope{}, err
	}
	agentName = strings.TrimSpace(agentName)
	subject = strings.TrimSpace(subject)
	if strings.TrimSpace(h.rt.PrincipalMode) == "remote" {
		if h.rt.AuthorizeAgentScope == nil {
			return k12storage.TextbookScope{}, errors.New("remote agent authorizer missing")
		}
		if err := h.rt.AuthorizeAgentScope(ctx, ownerID, agentName); err != nil {
			return k12storage.TextbookScope{}, err
		}
	}
	return k12storage.TextbookScope{
		OwnerID: ownerID, AgentName: agentName, Subject: subject,
	}, nil
}
