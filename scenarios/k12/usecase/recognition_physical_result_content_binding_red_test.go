package usecase

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// REG-K12-RECOGNIZING-POLICY-005: a syntactically valid result digest is not
// success evidence by itself. Even when the same forged SHA is copied into the
// child and the redacted recognition receipt, recovery must recompute it from
// the private provider content and keep the parent/Job parked on mismatch.
func TestREGK12RecognizingPolicy005RejectsSynchronizedForgedResultDigest(
	t *testing.T,
) {
	fixture := newDD036RecognitionReconcileFixture(
		t,
		"reg-k12-recognizing-policy-005-forged-result-digest",
		"1+1=",
	)

	const providerContentA = `[{"question":"1+1=","subject":"数学","answer_state":"blank"}]`
	const unrelatedContentB = `{"forged":"different provider content"}`
	forgedDigest := modelInvocationDigest([]byte(unrelatedContentB))

	child := fixture.addSucceededPhysicalChild(
		t,
		k12.RecognitionPhysicalUnitWholePage,
		fixture.run.req.Image,
		"",
		"",
		forgedDigest,
		providerContentA,
	)
	fixture.persistRawReceipt(
		t,
		recognitionPhysicalReceiptSet(
			[]k12.ModelPhysicalInvocation{child},
		),
	)
	fixture.park(t)

	reconciled, _, reconcileErr := fixture.reconcile(t)
	fixture.assertStillParked(t, reconciled, reconcileErr)
}

// REG-K12-RECOGNIZING-POLICY-005: the full fallback exact-set is valid only
// when its durable authorization still binds the exact succeeded whole-page
// content. Child and receipt consistency must not hide authorization drift.
func TestREGK12RecognizingPolicy005RejectsFallbackAuthorizationDigestDrift(
	t *testing.T,
) {
	photo := orchestratorPhotoRequest()
	photo.Image = dd036DenseReconcileImage(t)
	fixture := newDD036RecognitionReconcileFixtureWithPhoto(
		t,
		"reg-k12-recognizing-policy-005-authorization-drift",
		"1+1=",
		photo,
	)
	units := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	children := make([]k12.ModelPhysicalInvocation, 0, len(units))
	for index, unit := range units {
		payload := `[]`
		switch {
		case unit == k12.RecognitionPhysicalUnitWholePage:
			payload = `not-json`
		case index == 1:
			payload = `[{"question":"1+1=","subject":"数学","answer_state":"blank"}]`
		}
		children = append(
			children,
			fixture.addSucceededPhysicalChild(
				t,
				unit,
				dd036ReconcilePhysicalImage(fixture, unit),
				"",
				"",
				"",
				payload,
			),
		)
	}
	fixture.persistRawReceipt(
		t,
		recognitionPhysicalReceiptSet(children),
	)
	if _, err := fixture.deps.Records.DB().ExecContext(
		fixture.ctx,
		`UPDATE k12_recognition_fallback_authorizations
		    SET whole_result_digest=?
		  WHERE parent_invocation_id=?`,
		modelInvocationDigest([]byte("authorization-drift")),
		fixture.parent.InvocationID,
	); err != nil {
		t.Fatalf("inject fallback authorization digest drift: %v", err)
	}
	fixture.park(t)

	reconciled, _, reconcileErr := fixture.reconcile(t)
	fixture.assertStillParked(t, reconciled, reconcileErr)
}
