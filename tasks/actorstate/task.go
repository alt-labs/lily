package actorstate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-cid"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lily/chain/actors/builtin"
	"github.com/filecoin-project/lily/lens/task"
	"github.com/filecoin-project/lily/metrics"
	"github.com/filecoin-project/lily/model"
	visormodel "github.com/filecoin-project/lily/model/visor"
)

// A Task processes the extraction of actor state according the allowed types in its extracter map.
type Task struct {
	node task.TaskAPI

	extracterMap ActorExtractorMap

	apwg sync.WaitGroup
}

func NewTask(node task.TaskAPI, extracterMap ActorExtractorMap) *Task {
	p := &Task{
		node:         node,
		extracterMap: extracterMap,
	}
	return p
}

func (t *Task) ProcessActors(ctx context.Context, current *types.TipSet, executed *types.TipSet, candidates task.ActorStateChangeDiff) (model.Persistable, *visormodel.ProcessingReport, error) {
	log.Debugw("processing actor state changes", "height", current.Height(), "parent_height", executed.Height())

	// TODO this is the wrong height and stateroot
	// actors state exists in current and was produced by executing messages in executed
	report := &visormodel.ProcessingReport{
		Height:    int64(current.Height()),
		StateRoot: current.ParentState().String(),
		Status:    visormodel.ProcessingStatusOK,
	}

	ll := log.With("height", int64(current.Height()))

	// Filter to just allowed actors
	actors := make(map[address.Address]task.ActorStateChange)
	for addr, ch := range candidates {
		if t.extracterMap.Allow(ch.Actor.Code) {
			actors[addr] = ch
		}
	}

	data := make(model.PersistableList, 0, len(actors))
	errorsDetected := make([]*ActorStateError, 0, len(actors))
	skippedActors := 0

	if len(actors) == 0 {
		ll.Debugw("no actor state changes found")
		return data, report, nil
	}

	start := time.Now()
	ll.Debugw("found actor state changes", "count", len(actors))

	results := make(chan *ActorStateResult, len(actors))
	// closes result when done, runs extraction for each actor in actors concurrently.
	t.startActorStateExtraction(ctx, current, executed, actors, results)

	// Gather results
	for res := range results {
		select {
		case <-ctx.Done():
			return data, report, ctx.Err()
		default:
		}
		lla := log.With("height", int64(current.Height()), "actor", builtin.ActorNameByCode(res.Code), "address", res.Address)

		if res.Error != nil {
			lla.Errorw("actor returned with error", "error", res.Error.Error())
			errorsDetected = append(errorsDetected, &ActorStateError{
				Code:    res.Code.String(),
				Name:    builtin.ActorNameByCode(res.Code),
				Head:    res.Head.String(),
				Address: res.Address.String(),
				Error:   res.Error.Error(),
			})
			continue
		}

		if res.SkippedParse {
			lla.Debugw("skipped actor without extracter")
			skippedActors++
		}

		data = append(data, res.Data)
	}

	log.Debugw("completed processing actor state changes", "height", current.Height(), "success", len(actors)-len(errorsDetected)-skippedActors, "errors", len(errorsDetected), "skipped", skippedActors, "time", time.Since(start))

	if skippedActors > 0 {
		report.StatusInformation = fmt.Sprintf("did not parse %d actors", skippedActors)
	}

	if len(errorsDetected) != 0 {
		report.ErrorsDetected = errorsDetected
	}

	return data, report, nil
}

func (t *Task) startActorStateExtraction(ctx context.Context, current, executed *types.TipSet, actors task.ActorStateChangeDiff, results chan *ActorStateResult) {
	for addr, ac := range actors {
		addr := addr
		ac := ac

		t.apwg.Add(1)
		go func() {
			defer t.apwg.Done()

			ctx, _ = tag.New(ctx, tag.Upsert(metrics.ActorCode, builtin.ActorNameByCode(ac.Actor.Code)))

			res := &ActorStateResult{
				Code:    ac.Actor.Code,
				Head:    ac.Actor.Head,
				Address: addr,
			}

			info := ActorInfo{
				Actor:        ac.Actor,
				ChangeType:   ac.ChangeType,
				Address:      addr,
				TipSet:       current,
				ParentTipSet: executed,
			}
			extracter, ok := t.extracterMap.GetExtractor(info.Actor.Code)
			if !ok {
				res.SkippedParse = true
			} else {
				// Parse state
				data, err := extracter.Extract(ctx, info, t.node)
				if err != nil {
					res.Error = xerrors.Errorf("failed to extract parsed actor state: %w", err)
				}
				res.Data = data
			}
			results <- res
		}()
	}
	go func() {
		t.apwg.Wait()
		close(results)
	}()
}

type ActorStateResult struct {
	Code         cid.Cid
	Head         cid.Cid
	Address      address.Address
	Error        error
	SkippedParse bool
	Data         model.Persistable
}

type ActorStateError struct {
	Code    string
	Name    string
	Head    string
	Address string
	Error   string
}

// An ActorExtractorMap controls which actor types may be extracted.
type ActorExtractorMap interface {
	Allow(code cid.Cid) bool
	GetExtractor(code cid.Cid) (ActorStateExtractor, bool)
}

type ActorExtractorFilter interface {
	AllowAddress(addr string) bool
}

// A RawActorExtractorMap extracts all types of actors using basic actor extraction which only parses shallow state.
type RawActorExtractorMap struct{}

func (RawActorExtractorMap) Allow(code cid.Cid) bool {
	return true
}

func (RawActorExtractorMap) GetExtractor(code cid.Cid) (ActorStateExtractor, bool) {
	return ActorExtractor{}, true
}

// A TypedActorExtractorMap extracts a single type of actor using full parsing of actor state
type TypedActorExtractorMap struct {
	codes *cid.Set
}

func NewTypedActorExtractorMap(codes []cid.Cid) *TypedActorExtractorMap {
	t := &TypedActorExtractorMap{
		codes: cid.NewSet(),
	}
	for _, c := range codes {
		t.codes.Add(c)
	}
	return t
}

func (t *TypedActorExtractorMap) Allow(code cid.Cid) bool {
	return t.codes.Has(code)
}

func (t *TypedActorExtractorMap) GetExtractor(code cid.Cid) (ActorStateExtractor, bool) {
	if !t.Allow(code) {
		return nil, false
	}
	return GetActorStateExtractor(code)
}
