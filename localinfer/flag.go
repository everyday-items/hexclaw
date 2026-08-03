package localinfer

import "github.com/hexagon-codes/hexclaw/featureflag"

const FlagCoordinatorV1 = "local.inference.coordinator.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagCoordinatorV1,
		Default:      true,
		Description:  "Coordinate local chat, embedding, rerank, probe, and warmup on one physical resource boundary",
		Stage:        featureflag.StageBeta,
		SinceVersion: "0.5.0",
	})
}
