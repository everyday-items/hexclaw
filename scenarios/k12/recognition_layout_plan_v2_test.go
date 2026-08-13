package k12

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestREGK12RecognitionManifest20260808001DeterministicPlan(t *testing.T) {
	pagePNG := recognitionLayoutPlanTestPNG(t, 240, 220)
	manifest := RecognitionLayoutManifestSuccessV2{
		InvocationID: "invocation-layout-manifest-1",
		ResultDigest: "sha256:" + strings.Repeat("a", 64),
	}
	targets := recognitionLayoutPlanTestTargets(17)

	build := func(
		t *testing.T,
		page []byte,
		manifest RecognitionLayoutManifestSuccessV2,
		targets []RecognitionLayoutManifestTargetV2,
	) (RecognitionLayoutPlanV2, error) {
		t.Helper()
		return BuildRecognitionLayoutPlanV2(RecognitionLayoutPlanInputV2{
			PagePNG:  page,
			Manifest: manifest,
			Targets:  targets,
		})
	}

	t.Run("stable_exact_set_and_contact_sheets", func(t *testing.T) {
		first, err := build(t, pagePNG, manifest, targets)
		if err != nil {
			t.Fatalf("build first plan: %v", err)
		}
		secondTargets := append([]RecognitionLayoutManifestTargetV2(nil), targets...)
		for left, right := 0, len(secondTargets)-1; left < right; left, right = left+1, right-1 {
			secondTargets[left], secondTargets[right] = secondTargets[right], secondTargets[left]
		}
		second, err := build(t, append([]byte(nil), pagePNG...), manifest, secondTargets)
		if err != nil {
			t.Fatalf("build second plan: %v", err)
		}
		firstJSON, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("marshal first plan: %v", err)
		}
		secondJSON, err := json.Marshal(second)
		if err != nil {
			t.Fatalf("marshal second plan: %v", err)
		}
		if !bytes.Equal(firstJSON, secondJSON) {
			t.Fatalf("same source and manifest must produce byte-stable plans\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
		}
		if first.Version != RecognitionPlanVersionV2 {
			t.Fatalf("plan version=%d want=%d", first.Version, RecognitionPlanVersionV2)
		}
		for field, digest := range map[string]string{
			"page":            first.PageDigest,
			"authorized_plan": first.AuthorizedPlanDigest,
		} {
			assertRecognitionLayoutSHA256(t, field, digest)
		}
		if len(first.Targets) != 17 {
			t.Fatalf("target count=%d want=17", len(first.Targets))
		}
		if len(first.Batches) != 5 {
			t.Fatalf("batch count=%d want=5", len(first.Batches))
		}

		wantTargetIDs := make(map[string]struct{}, len(first.Targets))
		lastTop, lastLeft := -1, -1
		for index, target := range first.Targets {
			if target.TargetID == "" || strings.Contains(target.TargetID, targets[index].ManifestRef) {
				t.Fatalf("target %d does not have a server-derived opaque ID: %#v", index, target)
			}
			if _, exists := wantTargetIDs[target.TargetID]; exists {
				t.Fatalf("duplicate derived target ID %q", target.TargetID)
			}
			wantTargetIDs[target.TargetID] = struct{}{}
			assertRecognitionLayoutSHA256(t, fmt.Sprintf("candidate[%d]", index), target.CropDigest)
			if len(target.SourceNumberPath) != 1 || target.SourceNumberPath[0] == "" || target.DisplayLabel == "" {
				t.Fatalf("target %d lost validated source numbering: %#v", index, target)
			}
			if target.Region.Y < lastTop || (target.Region.Y == lastTop && target.Region.X < lastLeft) {
				t.Fatalf("targets are not in stable source-pixel top/left order at %d: %#v", index, first.Targets)
			}
			lastTop, lastLeft = target.Region.Y, target.Region.X
		}

		gotTargetIDs := make(map[string]struct{}, len(first.Targets))
		for index, batch := range first.Batches {
			wantUnit := RecognitionPhysicalUnit(fmt.Sprintf("layout_batch_%04d", index+1))
			if batch.Unit != wantUnit {
				t.Fatalf("batch %d unit=%q want=%q", index, batch.Unit, wantUnit)
			}
			if len(batch.TargetIDs) == 0 || len(batch.TargetIDs) > RecognitionLayoutBatchTargetLimitV2 {
				t.Fatalf("batch %q target count=%d want 1..%d", batch.Unit, len(batch.TargetIDs), RecognitionLayoutBatchTargetLimitV2)
			}
			for _, targetID := range batch.TargetIDs {
				if _, exists := gotTargetIDs[targetID]; exists {
					t.Fatalf("target %q occurs in more than one batch", targetID)
				}
				gotTargetIDs[targetID] = struct{}{}
			}
			assertRecognitionLayoutSHA256(t, string(batch.Unit), batch.InputDigest)
			imageOne, err := BuildRecognitionLayoutBatchImageV2(pagePNG, first, batch.Unit)
			if err != nil {
				t.Fatalf("build batch image %q: %v", batch.Unit, err)
			}
			imageTwo, err := BuildRecognitionLayoutBatchImageV2(pagePNG, second, batch.Unit)
			if err != nil {
				t.Fatalf("rebuild batch image %q: %v", batch.Unit, err)
			}
			if !bytes.Equal(imageOne, imageTwo) {
				t.Fatalf("batch image %q is not byte-stable", batch.Unit)
			}
			if got := recognitionLayoutTestDigest(imageOne); got != batch.InputDigest {
				t.Fatalf("batch %q input digest=%q want=%q", batch.Unit, batch.InputDigest, got)
			}
		}
		if len(gotTargetIDs) != len(wantTargetIDs) {
			t.Fatalf("batch union size=%d want=%d", len(gotTargetIDs), len(wantTargetIDs))
		}
		for targetID := range wantTargetIDs {
			if _, exists := gotTargetIDs[targetID]; !exists {
				t.Fatalf("target %q missing from batch exact-set", targetID)
			}
		}
	})

	t.Run("invalid_manifest_and_geometry_fail_closed", func(t *testing.T) {
		cases := []struct {
			name     string
			page     []byte
			manifest RecognitionLayoutManifestSuccessV2
			targets  []RecognitionLayoutManifestTargetV2
		}{
			{name: "zero targets", page: pagePNG, manifest: manifest, targets: nil},
			{name: "thirty three targets", page: pagePNG, manifest: manifest, targets: recognitionLayoutPlanTestTargets(33)},
			{name: "non canonical manifest ref", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[0].ManifestRef = "target-1" })},
			{name: "duplicate manifest ref", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[1].ManifestRef = items[0].ManifestRef })},
			{name: "duplicate manifest order", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[1].ManifestOrder = items[0].ManifestOrder })},
			{name: "manifest ref and order disagree", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) {
				items[0].ManifestRef, items[1].ManifestRef = items[1].ManifestRef, items[0].ManifestRef
			})},
			{name: "number path without label", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[0].DisplayLabel = "" })},
			{name: "zero width", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[0].Region.Width = 0 })},
			{name: "outside right edge", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[0].Region.X = 239; items[0].Region.Width = 2 })},
			{name: "outside bottom edge", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[0].Region.Y = 219; items[0].Region.Height = 2 })},
			{name: "negative origin", page: pagePNG, manifest: manifest, targets: mutateRecognitionLayoutTargets(targets, func(items []RecognitionLayoutManifestTargetV2) { items[0].Region.X = -1 })},
			{name: "empty invocation identity", page: pagePNG, manifest: RecognitionLayoutManifestSuccessV2{ResultDigest: manifest.ResultDigest}, targets: targets},
			{name: "non canonical result digest", page: pagePNG, manifest: RecognitionLayoutManifestSuccessV2{InvocationID: manifest.InvocationID, ResultDigest: "sha256:not-a-digest"}, targets: targets},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := build(t, tc.page, tc.manifest, tc.targets); err == nil {
					t.Fatal("invalid plan input was accepted")
				}
			})
		}
	})

	t.Run("repair_unit_is_strictly_four_digit", func(t *testing.T) {
		for _, ordinal := range []int{1, 9999} {
			unit, err := RecognitionLayoutRepairUnitV2(ordinal)
			if err != nil {
				t.Fatalf("repair ordinal %d: %v", ordinal, err)
			}
			if want := RecognitionPhysicalUnit(fmt.Sprintf("layout_repair_%04d", ordinal)); unit != want {
				t.Fatalf("repair ordinal %d unit=%q want=%q", ordinal, unit, want)
			}
		}
		for _, ordinal := range []int{-1, 0, 10000} {
			if _, err := RecognitionLayoutRepairUnitV2(ordinal); err == nil {
				t.Fatalf("non-four-digit repair ordinal %d was accepted", ordinal)
			}
		}
	})

	t.Run("singleton_repair_crop_is_byte_stable_and_fail_closed", func(t *testing.T) {
		plan, err := build(t, pagePNG, manifest, recognitionLayoutPlanTestTargets(2))
		if err != nil {
			t.Fatal(err)
		}
		first, err := BuildRecognitionLayoutRepairImageV2(
			pagePNG,
			plan,
			plan.Targets[1].TargetID,
		)
		if err != nil {
			t.Fatal(err)
		}
		second, err := BuildRecognitionLayoutRepairImageV2(
			append([]byte(nil), pagePNG...),
			plan,
			plan.Targets[1].TargetID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) ||
			recognitionLayoutTestDigest(first) != plan.Targets[1].CropDigest {
			t.Fatal("singleton repair did not reuse the canonical target crop")
		}
		decoded, err := png.Decode(bytes.NewReader(first))
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Bounds().Dx() != plan.Targets[1].Region.Width ||
			decoded.Bounds().Dy() != plan.Targets[1].Region.Height {
			t.Fatalf("repair crop bounds=%v want source region=%+v", decoded.Bounds(), plan.Targets[1].Region)
		}

		wrongPage := append([]byte(nil), pagePNG...)
		wrongPage[len(wrongPage)-1] ^= 1
		driftedPlan := plan
		driftedPlan.Targets = append([]RecognitionLayoutTargetV2(nil), plan.Targets...)
		driftedPlan.Targets[1].CropDigest = "sha256:" + strings.Repeat("f", 64)
		for name, test := range map[string]struct {
			page      []byte
			plan      RecognitionLayoutPlanV2
			candidate string
		}{
			"wrong canonical page": {page: wrongPage, plan: plan, candidate: plan.Targets[1].TargetID},
			"plan digest drift":    {page: pagePNG, plan: driftedPlan, candidate: plan.Targets[1].TargetID},
			"unknown candidate":    {page: pagePNG, plan: plan, candidate: "layout_target_v2_unknown"},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := BuildRecognitionLayoutRepairImageV2(
					test.page,
					test.plan,
					test.candidate,
				); err == nil {
					t.Fatal("unbound singleton repair crop was accepted")
				}
			})
		}
	})
}

func TestREGK12RecognitionManifest20260808001TargetIdentityUsesOnlyLocalSpatialFacts(
	t *testing.T,
) {
	pagePNG := recognitionLayoutPlanTestPNG(t, 240, 220)
	manifest := RecognitionLayoutManifestSuccessV2{
		InvocationID: "invocation-layout-target-identity-v2",
		ResultDigest: "sha256:" + strings.Repeat("b", 64),
	}
	build := func(
		t *testing.T,
		targets []RecognitionLayoutManifestTargetV2,
	) RecognitionLayoutPlanV2 {
		t.Helper()
		plan, err := BuildRecognitionLayoutPlanV2(RecognitionLayoutPlanInputV2{
			PagePNG: pagePNG, Manifest: manifest, Targets: targets,
		})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	baseTarget := RecognitionLayoutManifestTargetV2{
		ManifestRef:      "manifest_0001",
		ManifestOrder:    1,
		SourceNumberPath: []string{"一", "1"},
		DisplayLabel:     "一、1",
		Region: SourcePixelRegion{
			X: 80, Y: 80, Width: 48, Height: 32,
		},
	}
	base := build(t, []RecognitionLayoutManifestTargetV2{baseTarget})

	modelNumberingChanged := baseTarget
	modelNumberingChanged.SourceNumberPath = []string{"九", "9"}
	modelNumberingChanged.DisplayLabel = "九、9"
	changedEvidence := build(t, []RecognitionLayoutManifestTargetV2{modelNumberingChanged})
	if base.Targets[0].TargetID != changedEvidence.Targets[0].TargetID {
		t.Fatalf(
			"model numbering changed durable TargetID\nbase=%q\nchanged=%q",
			base.Targets[0].TargetID,
			changedEvidence.Targets[0].TargetID,
		)
	}
	if changedEvidence.Targets[0].DisplayLabel != "九、9" ||
		len(changedEvidence.Targets[0].SourceNumberPath) != 2 ||
		changedEvidence.Targets[0].SourceNumberPath[0] != "九" ||
		changedEvidence.Targets[0].SourceNumberPath[1] != "9" {
		t.Fatalf("plan lost changed model numbering evidence: %#v", changedEvidence.Targets[0])
	}
	if base.AuthorizedPlanDigest == changedEvidence.AuthorizedPlanDigest {
		t.Fatal("model numbering evidence did not change authorized plan digest")
	}

	bboxChanged := baseTarget
	bboxChanged.Region.X++
	changedBBox := build(t, []RecognitionLayoutManifestTargetV2{bboxChanged})
	if base.Targets[0].TargetID == changedBBox.Targets[0].TargetID {
		t.Fatal("canonical bbox change did not change durable TargetID")
	}

	prefix := RecognitionLayoutManifestTargetV2{
		ManifestRef:      "manifest_0001",
		ManifestOrder:    1,
		SourceNumberPath: []string{"0"},
		DisplayLabel:     "0.",
		Region: SourcePixelRegion{
			X: 8, Y: 8, Width: 48, Height: 32,
		},
	}
	spatiallySecond := baseTarget
	spatiallySecond.ManifestRef = "manifest_0002"
	spatiallySecond.ManifestOrder = 2
	changedOrdinal := build(t, []RecognitionLayoutManifestTargetV2{spatiallySecond, prefix})
	var sameBBoxSecondOrdinal *RecognitionLayoutTargetV2
	for index := range changedOrdinal.Targets {
		if changedOrdinal.Targets[index].Region == baseTarget.Region {
			sameBBoxSecondOrdinal = &changedOrdinal.Targets[index]
			break
		}
	}
	if sameBBoxSecondOrdinal == nil {
		t.Fatal("fixture lost the unchanged canonical bbox")
	}
	if base.Targets[0].TargetID == sameBBoxSecondOrdinal.TargetID {
		t.Fatal("local spatial ordinal change did not change durable TargetID")
	}
}

func recognitionLayoutPlanTestTargets(count int) []RecognitionLayoutManifestTargetV2 {
	targets := make([]RecognitionLayoutManifestTargetV2, 0, count)
	for index := count - 1; index >= 0; index-- {
		row, column := index/4, index%4
		targets = append(targets, RecognitionLayoutManifestTargetV2{
			ManifestRef:      fmt.Sprintf("manifest_%04d", index+1),
			ManifestOrder:    index + 1,
			SourceNumberPath: []string{fmt.Sprintf("%d", index+1)},
			DisplayLabel:     fmt.Sprintf("%d.", index+1),
			Region: SourcePixelRegion{
				X: 8 + column*56, Y: 8 + row*40,
				Width: 48, Height: 32,
			},
		})
	}
	return targets
}

func mutateRecognitionLayoutTargets(
	targets []RecognitionLayoutManifestTargetV2,
	mutate func([]RecognitionLayoutManifestTargetV2),
) []RecognitionLayoutManifestTargetV2 {
	cloned := append([]RecognitionLayoutManifestTargetV2(nil), targets...)
	mutate(cloned)
	return cloned
}

func recognitionLayoutPlanTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x*3 + y) % 251),
				G: uint8((x + y*5) % 253),
				B: uint8((x*7 + y*11) % 255),
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode page PNG: %v", err)
	}
	return encoded.Bytes()
}

func recognitionLayoutTestDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertRecognitionLayoutSHA256(t *testing.T, field, value string) {
	t.Helper()
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		t.Fatalf("%s digest=%q is not canonical sha256", field, value)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil {
		t.Fatalf("%s digest=%q is not hexadecimal: %v", field, value, err)
	}
}
