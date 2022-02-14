package actorstate

import (
	"context"

	"go.opentelemetry.io/otel"

	"github.com/filecoin-project/lily/chain/actors/builtin/reward"

	"github.com/filecoin-project/lily/metrics"
	"github.com/filecoin-project/lily/model"
	rewardmodel "github.com/filecoin-project/lily/model/actors/reward"
)

// was services/processor/tasks/reward/reward.go

// RewardExtractor extracts reward actor state
type RewardExtractor struct{}

func init() {
	for _, c := range reward.AllCodes() {
		Register(c, RewardExtractor{})
	}
}

func (RewardExtractor) Extract(ctx context.Context, a ActorInfo, node ActorStateAPI) (model.Persistable, error) {
	ctx, span := otel.Tracer("").Start(ctx, "RewardExtractor")
	defer span.End()

	stop := metrics.Timer(ctx, metrics.StateExtractionDuration)
	defer stop()

	rstate, err := reward.Load(node.Store(), &a.Actor)
	if err != nil {
		return nil, err
	}

	csbaseline, err := rstate.CumsumBaseline()
	if err != nil {
		return nil, err
	}

	csrealized, err := rstate.CumsumRealized()
	if err != nil {
		return nil, err
	}
	effectiveBaselinePower, err := rstate.EffectiveBaselinePower()
	if err != nil {
		return nil, err
	}

	thisBaslinePower, err := rstate.ThisEpochBaselinePower()
	if err != nil {
		return nil, err
	}

	thisRewardSmoothed, err := rstate.ThisEpochRewardSmoothed()
	if err != nil {
		return nil, err
	}

	totalMinedReward, err := rstate.TotalStoragePowerReward()
	if err != nil {
		return nil, err
	}

	thisReward, err := rstate.ThisEpochReward()
	if err != nil {
		return nil, err
	}

	networkTime, err := rstate.EffectiveNetworkTime()
	if err != nil {
		return nil, err
	}

	return &rewardmodel.ChainReward{
		Height:                            int64(a.TipSet.Height()),
		StateRoot:                         a.TipSet.ParentState().String(),
		CumSumBaseline:                    csbaseline.String(),
		CumSumRealized:                    csrealized.String(),
		EffectiveBaselinePower:            effectiveBaselinePower.String(),
		NewBaselinePower:                  thisBaslinePower.String(),
		NewRewardSmoothedPositionEstimate: thisRewardSmoothed.PositionEstimate.String(),
		NewRewardSmoothedVelocityEstimate: thisRewardSmoothed.VelocityEstimate.String(),
		TotalMinedReward:                  totalMinedReward.String(),
		NewReward:                         thisReward.String(),
		EffectiveNetworkTime:              int64(networkTime),
	}, nil
}
