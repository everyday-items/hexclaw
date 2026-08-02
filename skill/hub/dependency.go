package hub

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

var (
	// ErrSkillNotFound identifies a missing root or transitive dependency.
	ErrSkillNotFound = errors.New("hub skill not found")
	// ErrDependencyContract identifies ambiguous, cyclic, duplicated, or
	// non-skill dependency metadata.
	ErrDependencyContract = errors.New("invalid hub dependency contract")
)

// ResolveDependencies resolves a skill's dependency tree and returns
// the ordered list of skills to install (dependencies first).
//
// Returns error if circular dependency is detected.
func (h *Hub) ResolveDependencies(ctx context.Context, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	skills := h.skillIndexSnapshot()
	visited := make(map[string]bool)
	inStack := make(map[string]bool) // for cycle detection
	var order []string

	if err := resolveDFS(ctx, skills, name, visited, inStack, &order); err != nil {
		return nil, err
	}
	return order, nil
}

func resolveDFS(ctx context.Context, skills map[string]SkillMeta, name string, visited, inStack map[string]bool, order *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if visited[name] {
		return nil
	}
	if inStack[name] {
		return fmt.Errorf("%w: circular dependency at %s", ErrDependencyContract, name)
	}
	meta, ok := skills[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	if meta.Type != "" && meta.Type != "skill" {
		return fmt.Errorf("%w: %s has type %s", ErrDependencyContract, name, meta.Type)
	}
	deps, err := declaredDependencies(meta)
	if err != nil {
		return err
	}

	inStack[name] = true
	defer delete(inStack, name)

	for _, dep := range deps {
		if err := resolveDFS(ctx, skills, dep, visited, inStack, order); err != nil {
			return err
		}
	}

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
	skills := h.skillIndexSnapshot()
	var dependents []string
	for _, sk := range skills {
		deps, err := declaredDependencies(sk)
		if err != nil {
			continue
		}
		for _, dep := range deps {
			if dep == name {
				dependents = append(dependents, sk.Name)
				break
			}
		}
	}
	sort.Strings(dependents)
	return dependents
}

func (h *Hub) skillIndexSnapshot() map[string]SkillMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.catalog == nil {
		return nil
	}
	skills := make(map[string]SkillMeta, len(h.catalog.Skills))
	for _, skill := range h.catalog.Skills {
		skills[skill.Name] = cloneSkillMeta(skill)
	}
	return skills
}

func declaredDependencies(skill SkillMeta) ([]string, error) {
	if len(skill.Requires) > 0 && len(skill.Dependencies) > 0 && !slices.Equal(skill.Requires, skill.Dependencies) {
		return nil, fmt.Errorf("%w: %s declares conflicting requires and dependencies", ErrDependencyContract, skill.Name)
	}
	deps := skill.Requires
	if len(deps) == 0 {
		deps = skill.Dependencies
	}
	seen := make(map[string]struct{}, len(deps))
	for _, dependency := range deps {
		if strings.TrimSpace(dependency) == "" {
			return nil, fmt.Errorf("%w: %s has an empty dependency", ErrDependencyContract, skill.Name)
		}
		if _, exists := seen[dependency]; exists {
			return nil, fmt.Errorf("%w: %s repeats dependency %s", ErrDependencyContract, skill.Name, dependency)
		}
		seen[dependency] = struct{}{}
	}
	return slices.Clone(deps), nil
}
