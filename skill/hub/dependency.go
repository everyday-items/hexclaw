package hub

import (
	"context"
	"fmt"
	"strings"
)

// ResolveDependencies resolves a skill's dependency tree and returns
// the ordered list of skills to install (dependencies first).
//
// Returns error if circular dependency is detected.
func (h *Hub) ResolveDependencies(ctx context.Context, name string) ([]string, error) {
	visited := make(map[string]bool)
	inStack := make(map[string]bool) // for cycle detection
	var order []string

	if err := h.resolveDFS(ctx, name, visited, inStack, &order); err != nil {
		return nil, err
	}
	return order, nil
}

func (h *Hub) resolveDFS(ctx context.Context, name string, visited, inStack map[string]bool, order *[]string) error {
	if visited[name] {
		return nil
	}
	if inStack[name] {
		return fmt.Errorf("circular dependency detected: %s", name)
	}

	inStack[name] = true

	// Look up skill metadata to find dependencies
	deps := h.getSkillDeps(name)
	for _, dep := range deps {
		if err := h.resolveDFS(ctx, dep, visited, inStack, order); err != nil {
			return err
		}
	}

	inStack[name] = false
	visited[name] = true
	*order = append(*order, name)
	return nil
}

// InstallWithDependencies installs a skill and all its dependencies.
func (h *Hub) InstallWithDependencies(ctx context.Context, name string) ([]string, error) {
	order, err := h.ResolveDependencies(ctx, name)
	if err != nil {
		return nil, err
	}

	var installed []string
	for _, skillName := range order {
		if err := h.Install(ctx, skillName); err != nil {
			// If already installed, skip silently
			if strings.Contains(err.Error(), "already") {
				continue
			}
			return installed, fmt.Errorf("failed to install dependency %q: %w", skillName, err)
		}
		installed = append(installed, skillName)
	}
	return installed, nil
}

// ReverseDependencies returns skills that depend on the given skill.
func (h *Hub) ReverseDependencies(name string) []string {
	catalog := h.GetCatalog()
	if catalog == nil {
		return nil
	}

	var dependents []string
	for _, sk := range catalog.Skills {
		for _, dep := range h.getSkillDeps(sk.Name) {
			if dep == name {
				dependents = append(dependents, sk.Name)
				break
			}
		}
	}
	return dependents
}

// getSkillDeps returns the dependency list for a skill from catalog metadata.
func (h *Hub) getSkillDeps(name string) []string {
	catalog := h.GetCatalog()
	if catalog == nil {
		return nil
	}
	for _, sk := range catalog.Skills {
		if sk.Name == name {
			return sk.Dependencies
		}
	}
	return nil
}
