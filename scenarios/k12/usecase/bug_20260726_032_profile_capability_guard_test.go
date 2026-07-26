package usecase

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type bug20260726032ProfileStore struct {
	profile   k12.ChildProfile
	saveCalls int
}

func (s *bug20260726032ProfileStore) GetProfile(
	context.Context,
	string,
) (k12.ChildProfile, error) {
	return s.profile, nil
}

func (s *bug20260726032ProfileStore) SaveProfile(
	_ context.Context,
	_ string,
	profile k12.ChildProfile,
) error {
	s.saveCalls++
	s.profile = ApplyProfilePatchForBUG20260726032(s.profile, profile)
	return nil
}

func ApplyProfilePatchForBUG20260726032(
	current k12.ChildProfile,
	patch k12.ChildProfile,
) k12.ChildProfile {
	if patch.ChildName != "" {
		current.ChildName = patch.ChildName
	}
	if patch.GradeTerm != "" {
		current.GradeTerm = patch.GradeTerm
	}
	if patch.TextbookEdition != "" {
		current.TextbookEdition = patch.TextbookEdition
	}
	return current
}

func TestBUG20260726032_UpdateProfileCanonicalGuardHasNoRejectedWrite(t *testing.T) {
	store := &bug20260726032ProfileStore{profile: k12.ChildProfile{
		ChildName: "小明", GradeTerm: "六年级下", TextbookEdition: "人教版",
	}}
	deps := Deps{Profiles: store}
	ctx := context.Background()

	for _, gradeTerm := range []string{
		"初一", "初二", "初三", "高一", "高二", "高三",
		"初一上", "初一下", "初二上", "初二下", "初三上", "初三下",
		"高一上", "高一下", "高二上", "高二下", "高三上", "高三下",
	} {
		before := store.profile
		beforeCalls := store.saveCalls
		_, err := deps.UpdateProfile(ctx, "mingming", k12.ChildProfile{GradeTerm: gradeTerm})
		if err == nil {
			t.Fatalf("%s should be rejected", gradeTerm)
		}
		if !strings.Contains(err.Error(), "须为小学 12 档：一年级上～六年级下") {
			t.Fatalf("%s diagnostic unstable: %v", gradeTerm, err)
		}
		if store.saveCalls != beforeCalls || store.profile != before {
			t.Fatalf("%s caused persistence side effects: calls=%d profile=%+v",
				gradeTerm, store.saveCalls, store.profile)
		}
	}
}
