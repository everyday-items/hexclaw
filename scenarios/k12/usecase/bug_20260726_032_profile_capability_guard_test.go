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

func TestBUG20260726032_CanonicalProfileCreateAndEditAcceptEveryPrimaryGradeTerm(
	t *testing.T,
) {
	primary := []string{
		"一年级上", "一年级下", "二年级上", "二年级下", "三年级上", "三年级下",
		"四年级上", "四年级下", "五年级上", "五年级下", "六年级上", "六年级下",
	}
	for index, gradeTerm := range primary {
		store := &bug20260726032ProfileStore{}
		deps := Deps{Profiles: store}
		created, err := deps.UpdateProfile(
			context.Background(), "mingming", k12.ChildProfile{GradeTerm: gradeTerm},
		)
		if err != nil || created.GradeTerm != gradeTerm || store.saveCalls != 1 {
			t.Fatalf("create %s: profile=%+v saveCalls=%d err=%v",
				gradeTerm, created, store.saveCalls, err)
		}
		next := primary[(index+1)%len(primary)]
		edited, err := deps.UpdateProfile(
			context.Background(), "mingming", k12.ChildProfile{GradeTerm: next},
		)
		if err != nil || edited.GradeTerm != next || store.saveCalls != 2 {
			t.Fatalf("edit %s -> %s: profile=%+v saveCalls=%d err=%v",
				gradeTerm, next, edited, store.saveCalls, err)
		}
	}
}

func TestBUG20260726032_UpdateProfileCanonicalGuardHasNoRejectedWrite(t *testing.T) {
	store := &bug20260726032ProfileStore{profile: k12.ChildProfile{
		ChildName: "小明", GradeTerm: "六年级下", TextbookEdition: "人教版",
	}}
	deps := Deps{Profiles: store}
	ctx := context.Background()

	for _, gradeTerm := range []string{
		"初一", "初二", "初三", "高一", "高二", "高三",
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
