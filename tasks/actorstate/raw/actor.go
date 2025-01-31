package raw

import (
	"context"

	logging "github.com/ipfs/go-log/v2"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/filecoin-project/lily/chain/actors/builtin"
	"github.com/filecoin-project/lily/model"
	commonmodel "github.com/filecoin-project/lily/model/actors/common"
	"github.com/filecoin-project/lily/tasks/actorstate"
)

var log = logging.Logger("lily/tasks/rawactor")

// RawActorExtractor extracts common actor state
type RawActorExtractor struct{}

func (RawActorExtractor) Extract(ctx context.Context, a actorstate.ActorInfo, node actorstate.ActorStateAPI) (model.Persistable, error) {
	log.Debugw("Extract", zap.String("extractor", "RawActorExtractor"), zap.Inline(a))

	_, span := otel.Tracer("").Start(ctx, "RawActorExtractor.Extract")
	defer span.End()
	if span.IsRecording() {
		span.SetAttributes(a.Attributes()...)
	}

	return &commonmodel.Actor{
		Height:    int64(a.Current.Height()),
		ID:        a.Address.String(),
		StateRoot: a.Current.ParentState().String(),
		Code:      builtin.ActorNameByCode(a.Actor.Code),
		Head:      a.Actor.Head.String(),
		Balance:   a.Actor.Balance.String(),
		Nonce:     a.Actor.Nonce,
	}, nil
}
