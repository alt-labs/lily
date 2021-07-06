package chain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-hamt-ipld/v3"
	"github.com/filecoin-project/lotus/chain/state"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/sentinel-visor/lens/lotus"
	modelreg "github.com/filecoin-project/sentinel-visor/model/registry"
	init_2 "github.com/filecoin-project/sentinel-visor/tasks/actorstate/init_"
	miner2 "github.com/filecoin-project/sentinel-visor/tasks/actorstate/miner"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/otel/api/global"
	"go.opentelemetry.io/otel/label"
	"golang.org/x/xerrors"

	init_ "github.com/filecoin-project/sentinel-visor/chain/actors/builtin/init"
	"github.com/filecoin-project/sentinel-visor/chain/actors/builtin/market"
	"github.com/filecoin-project/sentinel-visor/chain/actors/builtin/miner"
	"github.com/filecoin-project/sentinel-visor/chain/actors/builtin/multisig"
	"github.com/filecoin-project/sentinel-visor/chain/actors/builtin/power"
	"github.com/filecoin-project/sentinel-visor/chain/actors/builtin/reward"
	"github.com/filecoin-project/sentinel-visor/lens"
	"github.com/filecoin-project/sentinel-visor/metrics"
	"github.com/filecoin-project/sentinel-visor/model"
	regmodels "github.com/filecoin-project/sentinel-visor/model/registry/registered"
	visormodel "github.com/filecoin-project/sentinel-visor/model/visor"
	"github.com/filecoin-project/sentinel-visor/tasks/actorstate"
	"github.com/filecoin-project/sentinel-visor/tasks/blocks"
	"github.com/filecoin-project/sentinel-visor/tasks/chaineconomics"
	"github.com/filecoin-project/sentinel-visor/tasks/messages"
	"github.com/filecoin-project/sentinel-visor/tasks/msapprovals"
)

var log = logging.Logger("visor/chain")

var _ TipSetObserver = (*TipSetIndexer)(nil)

// A TipSetWatcher waits for tipsets and persists their block data into a database.
type TipSetIndexer struct {
	window            time.Duration
	storage           model.Storage
	processors        map[string]TipSetProcessor
	messageProcessors map[string]MessageProcessor
	actorProcessors   map[string]ActorProcessor
	name              string
	persistSlot       chan struct{} // filled with a token when a gorroutine is persisting data
	lastTipSet        *types.TipSet
	node              lens.API
	opener            lens.APIOpener
	closer            lens.APICloser
	addressFilter     *AddressFilter
}

type TipSetIndexerOpt func(t *TipSetIndexer)

func AddressFilterOpt(f *AddressFilter) TipSetIndexerOpt {
	return func(t *TipSetIndexer) {
		t.addressFilter = f
	}
}

func ModelStringToType(ms string) (model.Persistable, error) {
	return regmodels.ModelForString(ms)
}

func ModelsForTopLevelTask(t string) (model.PersistableList, error) {
	return modelreg.ModelRegistry.ModelsForTask(t)
}

func ParseTaskString(name string) (string, model.PersistableList, error) {
	if strings.Contains(name, ":") {
		tokens := strings.Split(name, ":")
		if len(tokens) != 2 {
			// TODO don't do this
			panic("invalid task format? you fucked up, stop doing that")
		}
		taskName := tokens[0]
		taskModels := strings.Split(tokens[1], ",")
		log.Infow("task with subtasks created", "task", taskName, "subtasks", taskModels)
		var out model.PersistableList
		for _, tm := range taskModels {
			m, err := ModelStringToType(tm)
			if err != nil {
				return "", nil, err
			}
			out = append(out, m)
		}
		return taskName, out, nil
	} else {
		models, err := ModelsForTopLevelTask(name)
		if err != nil {
			return "", nil, err
		}
		switch name {
		case modelreg.BlocksTask:
			return name, models, nil
		case modelreg.MessagesTask:
			return name, models, nil
		case modelreg.ChainEconomicsTask:
			return name, models, nil
		case modelreg.ActorStatesRawTask:
			return name, models, nil
		case modelreg.ActorStatesPowerTask:
			return name, models, nil
		case modelreg.ActorStatesRewardTask:
			return name, models, nil
		case miner2.ActorStatesMinerTask:
			return name, models, nil
		case modelreg.ActorStatesInitTask:
			return name, models, nil
		case modelreg.ActorStatesMarketTask:
			return name, models, nil
		case modelreg.ActorStatesMultisigTask:
			return name, models, nil
		case modelreg.MultisigApprovalsTask:
			return name, models, nil
		default:
			return "", nil, xerrors.Errorf("unknown task: %s", name)
		}
	}
}

// A TipSetIndexer extracts block, message and actor state data from a tipset and persists it to storage. Extraction
// and persistence are concurrent. Extraction of the a tipset can proceed while data from the previous extraction is
// being persisted. The indexer may be given a time window in which to complete data extraction. The name of the
// indexer is used as the reporter in the visor_processing_reports table.
func NewTipSetIndexer(o lens.APIOpener, d model.Storage, window time.Duration, name string, tasks []string, options ...TipSetIndexerOpt) (*TipSetIndexer, error) {
	tsi := &TipSetIndexer{
		storage:           d,
		window:            window,
		name:              name,
		persistSlot:       make(chan struct{}, 1), // allow one concurrent persistence job
		processors:        map[string]TipSetProcessor{},
		messageProcessors: map[string]MessageProcessor{},
		actorProcessors:   map[string]ActorProcessor{},
		opener:            o,
	}

	for _, task := range tasks {
		taskName, models, err := ParseTaskString(task)
		if err != nil {
			return nil, err
		}
		switch taskName {
		case modelreg.BlocksTask:
			tsi.processors[modelreg.BlocksTask] = blocks.NewTask()
		case modelreg.MessagesTask:
			tsi.messageProcessors[modelreg.MessagesTask] = messages.NewTask()
		case modelreg.ChainEconomicsTask:
			tsi.processors[modelreg.ChainEconomicsTask] = chaineconomics.NewTask(o)
		case modelreg.ActorStatesRawTask:
			tsi.actorProcessors[modelreg.ActorStatesRawTask] = actorstate.NewTask(o, &actorstate.RawActorExtractorMap{})
		case modelreg.ActorStatesPowerTask:
			tsi.actorProcessors[modelreg.ActorStatesPowerTask] = actorstate.NewTask(o, actorstate.NewTypedActorExtractorMap(power.AllCodes()), models...)
		case modelreg.ActorStatesRewardTask:
			tsi.actorProcessors[modelreg.ActorStatesRewardTask] = actorstate.NewTask(o, actorstate.NewTypedActorExtractorMap(reward.AllCodes()), models...)
		case miner2.ActorStatesMinerTask:
			tsi.actorProcessors[miner2.ActorStatesMinerTask] = actorstate.NewTask(o, actorstate.NewTypedActorExtractorMap(miner.AllCodes()), models...)
		case init_2.ActorStatesInitTask:
			tsi.actorProcessors[modelreg.ActorStatesInitTask] = actorstate.NewTask(o, actorstate.NewTypedActorExtractorMap(init_.AllCodes()), models...)
		case modelreg.ActorStatesMarketTask:
			tsi.actorProcessors[modelreg.ActorStatesMarketTask] = actorstate.NewTask(o, actorstate.NewTypedActorExtractorMap(market.AllCodes()), models...)
		case modelreg.ActorStatesMultisigTask:
			tsi.actorProcessors[modelreg.ActorStatesMultisigTask] = actorstate.NewTask(o, actorstate.NewTypedActorExtractorMap(multisig.AllCodes()), models...)
		case modelreg.MultisigApprovalsTask:
			tsi.messageProcessors[modelreg.MultisigApprovalsTask] = msapprovals.NewTask(o)
		default:
			return nil, xerrors.Errorf("unknown task: %s", task)
		}
	}

	for _, opt := range options {
		opt(tsi)
	}

	return tsi, nil
}

// TipSet is called when a new tipset has been discovered
func (t *TipSetIndexer) TipSet(ctx context.Context, ts *types.TipSet) error {
	ctx, span := global.Tracer("").Start(ctx, "Indexer.TipSet")
	if span.IsRecording() {
		span.SetAttributes(label.String("tipset", ts.String()), label.Int64("height", int64(ts.Height())))
	}
	defer span.End()

	ctx, _ = tag.New(ctx, tag.Upsert(metrics.Name, t.name))

	var cancel func()
	var tctx context.Context // cancellable context for the task
	if t.window > 0 {
		// Do as much indexing as possible in the specified time window (usually one epoch when following head of chain)
		// Anything not completed in that time will be marked as incomplete
		tctx, cancel = context.WithTimeout(ctx, t.window)
	} else {
		// Ensure all goroutines are stopped when we exit
		tctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	ll := log.With("height", int64(ts.Height()))

	start := time.Now()

	inFlight := 0
	results := make(chan *TaskResult, len(t.processors)+len(t.actorProcessors))

	// A map to gather the persistable outputs from each task
	taskOutputs := make(map[string]model.PersistableList, len(t.processors)+len(t.actorProcessors))

	// Run each tipset processing task concurrently
	for name, p := range t.processors {
		inFlight++
		go t.runProcessor(tctx, p, name, ts, results)
	}

	// Run each actor or message processing task concurrently if we have any and we've seen a previous tipset to compare with
	if len(t.actorProcessors) > 0 || len(t.messageProcessors) > 0 {

		// Actor processors perform a diff between two tipsets so we need to keep track of parent and child
		var parent, child *types.TipSet
		if t.lastTipSet != nil {
			if t.lastTipSet.Height() > ts.Height() {
				// last tipset seen was the child
				child = t.lastTipSet
				parent = ts
			} else if t.lastTipSet.Height() < ts.Height() {
				// last tipset seen was the parent
				child = ts
				parent = t.lastTipSet
			} else {
				log.Errorw("out of order tipsets", "height", ts.Height(), "last_height", t.lastTipSet.Height())
			}
		}

		// If no parent tipset available then we need to skip processing. It's likely we received the last or first tipset
		// in a batch. No report is generated because a different run of the indexer could cover the parent and child
		// for this tipset.
		if parent != nil {
			if t.node == nil {
				node, closer, err := t.opener.Open(ctx)
				if err != nil {
					return xerrors.Errorf("unable to open lens: %w", err)
				}
				t.node = node
				t.closer = closer
			}

			// If we have message processors then extract the messages and receipts
			if len(t.messageProcessors) > 0 {
				tsMsgs, err := t.node.GetExecutedAndBlockMessagesForTipset(ctx, child, parent)
				if err == nil {
					// Start all the message processors
					for name, p := range t.messageProcessors {
						inFlight++
						go t.runMessageProcessor(tctx, p, name, child, parent, tsMsgs.Executed, tsMsgs.Block, results)
					}
				} else {
					ll.Errorw("failed to extract messages", "error", err)
					terr := xerrors.Errorf("failed to extract messages: %w", err)
					// We need to report that all message tasks failed
					for name := range t.messageProcessors {
						report := &visormodel.ProcessingReport{
							Height:         int64(ts.Height()),
							StateRoot:      ts.ParentState().String(),
							Reporter:       t.name,
							Task:           name,
							StartedAt:      start,
							CompletedAt:    time.Now(),
							Status:         visormodel.ProcessingStatusError,
							ErrorsDetected: terr,
						}
						taskOutputs[name] = model.PersistableList{report}
					}
				}
			}

			// If we have actor processors then find actors that have changed state
			if len(t.actorProcessors) > 0 {
				var err error
				var changes map[string]types.Actor
				// special case, we want to extract all actor states from the genesis block.
				if parent.Height() == 0 {
					changes, err = t.getGenesisActors(ctx)
				} else {
					changes, err = t.stateChangedActors(tctx, parent.ParentState(), child.ParentState())
				}
				if err == nil {
					if t.addressFilter != nil {
						for addr := range changes {
							if !t.addressFilter.Allow(addr) {
								delete(changes, addr)
							}
						}
					}
					for name, p := range t.actorProcessors {
						inFlight++
						go t.runActorProcessor(tctx, p, name, child, parent, changes, results)
					}
				} else {
					ll.Errorw("failed to extract actor changes", "error", err)
					terr := xerrors.Errorf("failed to extract actor changes: %w", err)
					// We need to report that all actor tasks failed
					for name := range t.actorProcessors {
						report := &visormodel.ProcessingReport{
							Height:         int64(ts.Height()),
							StateRoot:      ts.ParentState().String(),
							Reporter:       t.name,
							Task:           name,
							StartedAt:      start,
							CompletedAt:    time.Now(),
							Status:         visormodel.ProcessingStatusError,
							ErrorsDetected: terr,
						}
						taskOutputs[name] = model.PersistableList{report}
					}
				}
			}
		}
	}

	// Wait for all tasks to complete
	for inFlight > 0 {
		var res *TaskResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res = <-results:
		}
		inFlight--

		llt := ll.With("task", res.Task)

		// Was there a fatal error?
		if res.Error != nil {
			llt.Errorw("task returned with error", "error", res.Error.Error())
			// tell all the processors to close their connections to the lens, they can reopen when needed
			if err := t.closeProcessors(); err != nil {
				log.Errorw("error received while closing tipset indexer", "error", err)
			}
			return res.Error
		}

		if res.Report == nil {
			// Nothing was done for this tipset
			llt.Debugw("task returned with no report")
			continue
		}

		// Fill in some report metadata
		res.Report.Reporter = t.name
		res.Report.Task = res.Task
		res.Report.StartedAt = start
		res.Report.CompletedAt = time.Now()

		if res.Report.ErrorsDetected != nil {
			res.Report.Status = visormodel.ProcessingStatusError
		} else if res.Report.StatusInformation != "" {
			res.Report.Status = visormodel.ProcessingStatusInfo
		} else {
			res.Report.Status = visormodel.ProcessingStatusOK
		}

		llt.Infow("task report", "status", res.Report.Status, "time", res.Report.CompletedAt.Sub(res.Report.StartedAt))

		// Persist the processing report and the data in a single transaction
		taskOutputs[res.Task] = model.PersistableList{res.Report, res.Data}
	}

	// remember the last tipset we observed
	t.lastTipSet = ts

	if len(taskOutputs) == 0 {
		// Nothing to persist
		ll.Debugw("tipset complete, nothing to persist", "total_time", time.Since(start))
		return nil
	}

	// wait until there is an empty slot before persisting
	ll.Debugw("waiting to persist data", "time", time.Since(start))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case t.persistSlot <- struct{}{}:
		// Slot was free so we can continue. Slot is now taken.
	}

	// Persist all results
	go func() {
		// free up the slot when done
		defer func() {
			<-t.persistSlot
		}()

		ll.Debugw("persisting data", "time", time.Since(start))
		var wg sync.WaitGroup
		wg.Add(len(taskOutputs))

		// Persist each processor's data concurrently since they don't overlap
		for task, p := range taskOutputs {
			go func(task string, p model.Persistable) {
				defer wg.Done()
				ctx, _ = tag.New(ctx, tag.Upsert(metrics.TaskType, task))

				if err := t.storage.PersistBatch(ctx, p); err != nil {
					stats.Record(ctx, metrics.PersistFailure.M(1))
					ll.Errorw("persistence failed", "task", task, "error", err)
					return
				}
				ll.Debugw("task data persisted", "task", task, "time", time.Since(start))
			}(task, p)
		}
		wg.Wait()
		ll.Debugw("tipset complete", "total_time", time.Since(start))
	}()

	return nil
}

func (t *TipSetIndexer) runProcessor(ctx context.Context, p TipSetProcessor, name string, ts *types.TipSet, results chan *TaskResult) {
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.TaskType, name))
	stats.Record(ctx, metrics.TipsetHeight.M(int64(ts.Height())))
	stop := metrics.Timer(ctx, metrics.ProcessingDuration)
	defer stop()

	data, report, err := p.ProcessTipSet(ctx, ts)
	if err != nil {
		stats.Record(ctx, metrics.ProcessingFailure.M(1))
		results <- &TaskResult{
			Task:  name,
			Error: err,
		}
		return
	}
	results <- &TaskResult{
		Task:   name,
		Report: report,
		Data:   data,
	}
}

// getGenesisActors returns a map of all actors contained in the genesis block.
func (t *TipSetIndexer) getGenesisActors(ctx context.Context) (map[string]types.Actor, error) {
	out := map[string]types.Actor{}

	genesis, err := t.node.ChainGetGenesis(ctx)
	if err != nil {
		return nil, err
	}
	root, _, err := getStateTreeMapCIDAndVersion(ctx, t.node.Store(), genesis.ParentState())
	if err != nil {
		return nil, err
	}
	tree, err := state.LoadStateTree(t.node.Store(), root)
	if err != nil {
		return nil, err
	}
	if err := tree.ForEach(func(addr address.Address, act *types.Actor) error {
		out[addr.String()] = *act
		return nil

	}); err != nil {
		return nil, err
	}
	return out, nil
}

// stateChangedActors is an optimized version of the lotus API method StateChangedActors. This method takes advantage of the efficient hamt/v3 diffing logic
// and applies it to versions of state tress supporting it. These include Version 2 and 3 of the lotus state tree implementation.
// stateChangedActors will fall back to the lotus API method when the optimized diffing cannot be applied.
func (t *TipSetIndexer) stateChangedActors(ctx context.Context, old, new cid.Cid) (map[string]types.Actor, error) {
	ctx, span := global.Tracer("").Start(ctx, "StateChangedActors")
	if span.IsRecording() {
		span.SetAttributes(label.String("old", old.String()), label.String("new", new.String()))
	}
	defer span.End()

	var (
		buf = bytes.NewReader(nil)
		out = map[string]types.Actor{}
	)

	oldRoot, oldVersion, err := getStateTreeMapCIDAndVersion(ctx, t.node.Store(), old)
	if err != nil {
		return nil, err
	}
	newRoot, newVersion, err := getStateTreeMapCIDAndVersion(ctx, t.node.Store(), new)
	if err != nil {
		return nil, err
	}

	// efficient HAMT diffing does not work over the API lens
	_, isLotusAPILens := t.node.(*lotus.APIWrapper)
	if !isLotusAPILens && newVersion == oldVersion && (newVersion != types.StateTreeVersion0 && newVersion != types.StateTreeVersion1) {
		if span.IsRecording() {
			span.SetAttribute("diff", "fast")
		}
		changes, err := hamt.Diff(ctx, t.node.Store(), t.node.Store(), oldRoot, newRoot, hamt.UseTreeBitWidth(5), hamt.UseHashFunction(func(input []byte) []byte {
			res := sha256.Sum256(input)
			return res[:]
		}))
		if err != nil {
			log.Errorw("failed to diff state tree efficiently, falling back to slow method", "error", err)
		} else {
			if span.IsRecording() {
				span.SetAttribute("diff", "fast")
			}
			for _, change := range changes {
				addr, err := address.NewFromBytes([]byte(change.Key))
				if err != nil {
					return nil, xerrors.Errorf("address in state tree was not valid: %w", err)
				}
				var act types.Actor
				switch change.Type {
				case hamt.Add:
					buf.Reset(change.After.Raw)
					err = act.UnmarshalCBOR(buf)
					buf.Reset(nil)
					if err != nil {
						return nil, err
					}
				case hamt.Remove:
					buf.Reset(change.Before.Raw)
					err = act.UnmarshalCBOR(buf)
					buf.Reset(nil)
					if err != nil {
						return nil, err
					}
				case hamt.Modify:
					buf.Reset(change.After.Raw)
					err = act.UnmarshalCBOR(buf)
					buf.Reset(nil)
					if err != nil {
						return nil, err
					}
				}
				out[addr.String()] = act
			}
			return out, nil
		}
	}
	return t.node.StateChangedActors(ctx, old, new)
}

func (t *TipSetIndexer) runMessageProcessor(ctx context.Context, p MessageProcessor, name string, ts, pts *types.TipSet, emsgs []*lens.ExecutedMessage, blkMsgs []*lens.BlockMessages, results chan *TaskResult) {
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.TaskType, name))
	stats.Record(ctx, metrics.TipsetHeight.M(int64(ts.Height())))
	stop := metrics.Timer(ctx, metrics.ProcessingDuration)
	defer stop()

	data, report, err := p.ProcessMessages(ctx, ts, pts, emsgs, blkMsgs)
	if err != nil {
		stats.Record(ctx, metrics.ProcessingFailure.M(1))
		results <- &TaskResult{
			Task:  name,
			Error: err,
		}
		return
	}
	results <- &TaskResult{
		Task:   name,
		Report: report,
		Data:   data,
	}
}

func (t *TipSetIndexer) runActorProcessor(ctx context.Context, p ActorProcessor, name string, ts, pts *types.TipSet, actors map[string]types.Actor, results chan *TaskResult) {
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.TaskType, name))
	stats.Record(ctx, metrics.TipsetHeight.M(int64(ts.Height())))
	stop := metrics.Timer(ctx, metrics.ProcessingDuration)
	defer stop()

	data, report, err := p.ProcessActors(ctx, ts, pts, actors)
	if err != nil {
		stats.Record(ctx, metrics.ProcessingFailure.M(1))
		results <- &TaskResult{
			Task:  name,
			Error: err,
		}
		return
	}
	results <- &TaskResult{
		Task:   name,
		Report: report,
		Data:   data,
	}
}

func (t *TipSetIndexer) closeProcessors() error {
	if t.closer != nil {
		t.closer()
		t.closer = nil
	}
	t.node = nil

	for name, p := range t.processors {
		if err := p.Close(); err != nil {
			log.Errorw("error received while closing task processor", "error", err, "task", name)
		}
	}
	for name, p := range t.messageProcessors {
		if err := p.Close(); err != nil {
			log.Errorw("error received while closing message task processor", "error", err, "task", name)
		}
	}
	for name, p := range t.actorProcessors {
		if err := p.Close(); err != nil {
			log.Errorw("error received while closing actor task processor", "error", err, "task", name)
		}
	}

	return nil
}

func (t *TipSetIndexer) Close() error {
	log.Debug("closing tipset indexer")

	// We need to ensure that any persistence goroutine has completed. Since the channel has capacity 1 we can detect
	// when the persistence goroutine is running by attempting to send a probe value on the channel. When the channel
	// contains a token then we are still persisting and we should wait for that to be done.
	select {
	case t.persistSlot <- struct{}{}:
		// no token was in channel so there was no persistence goroutine running
	default:
		// channel contained a token so persistence goroutine is running
		// wait for the persistence to finish, which is when the channel can be sent on
		log.Debug("waiting for persistence to complete")
		t.persistSlot <- struct{}{}
		log.Debug("persistence completed")
	}

	// When we reach here there will always be a single token in the channel (our probe) which needs to be drained so
	// the channel is empty for reuse.
	<-t.persistSlot

	return t.closeProcessors()
}

// A TaskResult is either some data to persist or an error which indicates that the task did not complete. Partial
// completions are possible provided the Data contains a persistable log of the results.
type TaskResult struct {
	Task   string
	Error  error
	Report *visormodel.ProcessingReport
	Data   model.Persistable
}

type TipSetProcessor interface {
	// ProcessTipSet processes a tipset. If error is non-nil then the processor encountered a fatal error.
	// Any data returned must be accompanied by a processing report.
	ProcessTipSet(ctx context.Context, ts *types.TipSet) (model.Persistable, *visormodel.ProcessingReport, error)
	Close() error
}

type MessageProcessor interface {
	// ProcessMessages processes messages contained within a tipset. If error is non-nil then the processor encountered a fatal error.
	// pts is the tipset containing the messages, ts is the tipset containing the receipts
	// Any data returned must be accompanied by a processing report.
	ProcessMessages(ctx context.Context, ts *types.TipSet, pts *types.TipSet, emsgs []*lens.ExecutedMessage, blkMsgs []*lens.BlockMessages) (model.Persistable, *visormodel.ProcessingReport, error)
	Close() error
}

type ActorProcessor interface {
	// ProcessActor processes a set of actors. If error is non-nil then the processor encountered a fatal error.
	// Any data returned must be accompanied by a processing report.
	ProcessActors(ctx context.Context, ts *types.TipSet, pts *types.TipSet, actors map[string]types.Actor) (model.Persistable, *visormodel.ProcessingReport, error)
	Close() error
}
