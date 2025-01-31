package lily

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/events"
	"github.com/filecoin-project/lotus/chain/stmgr"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/node/impl/common"
	"github.com/filecoin-project/lotus/node/impl/full"
	"github.com/filecoin-project/lotus/node/impl/net"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
	"github.com/go-pg/pg/v10"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"go.uber.org/fx"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lily/chain/datasource"
	"github.com/filecoin-project/lily/chain/indexer"
	"github.com/filecoin-project/lily/lens/lily/modules"

	"github.com/filecoin-project/lily/chain"
	"github.com/filecoin-project/lily/lens"
	"github.com/filecoin-project/lily/lens/util"
	"github.com/filecoin-project/lily/network"
	"github.com/filecoin-project/lily/schedule"
	"github.com/filecoin-project/lily/storage"
)

var _ LilyAPI = (*LilyNodeAPI)(nil)

type LilyNodeAPI struct {
	fx.In `ignore-unexported:"true"`

	net.NetAPI
	full.ChainAPI
	full.StateAPI
	full.SyncAPI
	common.CommonAPI
	Events         *events.Events
	Scheduler      *schedule.Scheduler
	StorageCatalog *storage.Catalog
	ExecMonitor    stmgr.ExecMonitor
	CacheConfig    *util.CacheConfig
	actorStore     adt.Store
	actorStoreInit sync.Once
}

func (m *LilyNodeAPI) CirculatingSupply(ctx context.Context, key types.TipSetKey) (api.CirculatingSupply, error) {
	return m.StateAPI.StateVMCirculatingSupplyInternal(ctx, key)
}

func (m *LilyNodeAPI) ChainGetTipSetAfterHeight(ctx context.Context, epoch abi.ChainEpoch, key types.TipSetKey) (*types.TipSet, error) {
	// TODO (Frrist): I copied this from lotus, I need it now to handle gap filling edge cases.
	ts, err := m.ChainAPI.Chain.GetTipSetFromKey(ctx, key)
	if err != nil {
		return nil, xerrors.Errorf("loading tipset %s: %w", key, err)
	}
	return m.ChainAPI.Chain.GetTipsetByHeight(ctx, epoch, ts, false)
}

func (m *LilyNodeAPI) LilyIndex(_ context.Context, cfg *LilyIndexConfig) (interface{}, error) {
	md := storage.Metadata{
		JobName: cfg.Name,
	}
	// the context's passed to these methods live for the duration of the clients request, so make a new one.
	ctx := context.Background()

	// create a database connection for this watch, ensure its pingable, and run migrations if needed/configured to.
	strg, err := m.StorageCatalog.Connect(ctx, cfg.Storage, md)
	if err != nil {
		return nil, err
	}

	taskAPI, err := datasource.NewDataSource(m)
	if err != nil {
		return nil, err
	}

	// instantiate an indexer to extract block, message, and actor state data from observed tipsets and persists it to the storage.
	im, err := indexer.NewManager(taskAPI, strg, cfg.Name, cfg.Tasks, indexer.WithWindow(cfg.Window))
	if err != nil {
		return nil, err
	}

	ts, err := m.ChainGetTipSet(ctx, cfg.TipSet)
	if err != nil {
		return nil, err
	}

	success, err := im.TipSet(ctx, ts)

	return success, err

}

type watcherAPIWrapper struct {
	*events.Events
	full.ChainModuleAPI
}

func (m *LilyNodeAPI) LilyWatch(_ context.Context, cfg *LilyWatchConfig) (*schedule.JobSubmitResult, error) {
	// the context's passed to these methods live for the duration of the clients request, so make a new one.
	ctx := context.Background()

	md := storage.Metadata{
		JobName: cfg.Name,
	}

	// create a database connection for this watch, ensure its pingable, and run migrations if needed/configured to.
	strg, err := m.StorageCatalog.Connect(ctx, cfg.Storage, md)
	if err != nil {
		return nil, err
	}

	taskAPI, err := datasource.NewDataSource(m)
	if err != nil {
		return nil, err
	}

	// instantiate an indexer to extract block, message, and actor state data from observed tipsets and persists it to the storage.
	im, err := indexer.NewManager(taskAPI, strg, cfg.Name, cfg.Tasks, indexer.WithWindow(cfg.Window))
	if err != nil {
		return nil, err
	}

	res := m.Scheduler.Submit(&schedule.JobConfig{
		Name: cfg.Name,
		Type: "watch",
		Params: map[string]string{
			"window":     cfg.Window.String(),
			"storage":    cfg.Storage,
			"confidence": strconv.Itoa(cfg.Confidence),
			"worker":     strconv.Itoa(cfg.Workers),
			"buffer":     strconv.Itoa(cfg.BufferSize),
		},
		Tasks: cfg.Tasks,
		Job: chain.NewWatcher(&watcherAPIWrapper{
			Events:         m.Events,
			ChainModuleAPI: m.ChainModuleAPI,
		}, im, cfg.Name, cfg.Confidence, cfg.Workers, cfg.BufferSize),
		RestartOnFailure:    cfg.RestartOnFailure,
		RestartOnCompletion: cfg.RestartOnCompletion,
		RestartDelay:        cfg.RestartDelay,
	})

	return res, nil
}

func (m *LilyNodeAPI) LilyWalk(_ context.Context, cfg *LilyWalkConfig) (*schedule.JobSubmitResult, error) {
	// the context's passed to these methods live for the duration of the clients request, so make a new one.
	ctx := context.Background()

	md := storage.Metadata{
		JobName: cfg.Name,
	}

	// create a database connection for this watch, ensure its pingable, and run migrations if needed/configured to.
	strg, err := m.StorageCatalog.Connect(ctx, cfg.Storage, md)
	if err != nil {
		return nil, err
	}

	taskAPI, err := datasource.NewDataSource(m)
	if err != nil {
		return nil, err
	}

	// instantiate an indexer to extract block, message, and actor state data from observed tipsets and persists it to the storage.
	im, err := indexer.NewManager(taskAPI, strg, cfg.Name, cfg.Tasks, indexer.WithWindow(cfg.Window))
	if err != nil {
		return nil, err
	}

	res := m.Scheduler.Submit(&schedule.JobConfig{
		Name: cfg.Name,
		Type: "walk",
		Params: map[string]string{
			"window":    cfg.Window.String(),
			"minHeight": fmt.Sprintf("%d", cfg.From),
			"maxHeight": fmt.Sprintf("%d", cfg.To),
			"storage":   cfg.Storage,
		},
		Tasks:               cfg.Tasks,
		Job:                 chain.NewWalker(im, m, cfg.Name, cfg.From, cfg.To),
		RestartOnFailure:    cfg.RestartOnFailure,
		RestartOnCompletion: cfg.RestartOnCompletion,
		RestartDelay:        cfg.RestartDelay,
	})

	return res, nil
}

func (m *LilyNodeAPI) LilyGapFind(_ context.Context, cfg *LilyGapFindConfig) (*schedule.JobSubmitResult, error) {
	// the context's passed to these methods live for the duration of the clients request, so make a new one.
	ctx := context.Background()

	md := storage.Metadata{
		JobName: cfg.Name,
	}

	// create a database connection for this watch, ensure its pingable, and run migrations if needed/configured to.
	db, err := m.StorageCatalog.ConnectAsDatabase(ctx, cfg.Storage, md)
	if err != nil {
		return nil, err
	}

	res := m.Scheduler.Submit(&schedule.JobConfig{
		Name:  cfg.Name,
		Type:  "Find",
		Tasks: cfg.Tasks,
		Params: map[string]string{
			"minHeight": fmt.Sprintf("%d", cfg.From),
			"maxHeight": fmt.Sprintf("%d", cfg.To),
			"storage":   cfg.Storage,
		},
		Job:                 chain.NewGapIndexer(m, db, cfg.Name, cfg.From, cfg.To, cfg.Tasks),
		RestartOnFailure:    cfg.RestartOnFailure,
		RestartOnCompletion: cfg.RestartOnCompletion,
		RestartDelay:        cfg.RestartDelay,
	})

	return res, nil
}

func (m *LilyNodeAPI) LilyGapFill(_ context.Context, cfg *LilyGapFillConfig) (*schedule.JobSubmitResult, error) {
	// the context's passed to these methods live for the duration of the clients request, so make a new one.
	ctx := context.Background()

	md := storage.Metadata{
		JobName: cfg.Name,
	}

	// create a database connection for this watch, ensure its pingable, and run migrations if needed/configured to.
	db, err := m.StorageCatalog.ConnectAsDatabase(ctx, cfg.Storage, md)
	if err != nil {
		return nil, err
	}

	res := m.Scheduler.Submit(&schedule.JobConfig{
		Name: cfg.Name,
		Type: "Fill",
		Params: map[string]string{
			"minHeight": fmt.Sprintf("%d", cfg.From),
			"maxHeight": fmt.Sprintf("%d", cfg.To),
			"storage":   cfg.Storage,
		},
		Tasks:               cfg.Tasks,
		Job:                 chain.NewGapFiller(m, db, cfg.Name, cfg.From, cfg.To, cfg.Tasks),
		RestartOnFailure:    cfg.RestartOnFailure,
		RestartOnCompletion: cfg.RestartOnCompletion,
		RestartDelay:        cfg.RestartDelay,
	})

	return res, nil
}

func (m *LilyNodeAPI) LilyJobStart(_ context.Context, ID schedule.JobID) error {
	if err := m.Scheduler.StartJob(ID); err != nil {
		return err
	}
	return nil
}

func (m *LilyNodeAPI) LilyJobStop(_ context.Context, ID schedule.JobID) error {
	if err := m.Scheduler.StopJob(ID); err != nil {
		return err
	}
	return nil
}

// TODO pass context to fix wait
func (m *LilyNodeAPI) LilyJobWait(ctx context.Context, ID schedule.JobID) (*schedule.JobListResult, error) {
	res, err := m.Scheduler.WaitJob(ID)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (m *LilyNodeAPI) LilyJobList(_ context.Context) ([]schedule.JobListResult, error) {
	return m.Scheduler.Jobs(), nil
}

func (m *LilyNodeAPI) GetExecutedAndBlockMessagesForTipset(ctx context.Context, ts, pts *types.TipSet) (*lens.TipSetMessages, error) {
	return util.GetExecutedAndBlockMessagesForTipset(ctx, m.ChainAPI.Chain, m.StateManager, ts, pts)
}

func (m *LilyNodeAPI) GetMessageExecutionsForTipSet(ctx context.Context, next *types.TipSet, current *types.TipSet) ([]*lens.MessageExecution, error) {
	// this is defined in the lily daemon dep injection constructor, failure here is a developer error.
	msgMonitor, ok := m.ExecMonitor.(*modules.BufferedExecMonitor)
	if !ok {
		panic(fmt.Sprintf("bad cast, developer error expected modules.BufferedExecMonitor, got %T", m.ExecMonitor))
	}

	// if lily was watching the chain when this tipset was applied then its exec monitor will already
	// contain executions for this tipset.
	executions, err := msgMonitor.ExecutionFor(current)
	if err != nil {
		if err == modules.ExecutionTraceNotFound {
			// if lily hasn't watched this tipset be applied then we need to compute its execution trace.
			// this will likely be the case for most walk tasks.
			_, err := m.StateManager.ExecutionTraceWithMonitor(ctx, current, msgMonitor)
			if err != nil {
				return nil, xerrors.Errorf("failed to compute execution trace for tipset %s: %w", current.Key().String(), err)
			}
			// the above call will populate the msgMonitor with an execution trace for this tipset, get it.
			executions, err = msgMonitor.ExecutionFor(current)
			if err != nil {
				return nil, xerrors.Errorf("failed to find execution trace for tipset %s: %w", current.Key().String(), err)
			}
		} else {
			return nil, xerrors.Errorf("failed to extract message execution for tipset %s: %w", next, err)
		}
	}

	getActorCode, err := util.MakeGetActorCodeFunc(ctx, m.ChainAPI.Chain.ActorStore(ctx), next, current)
	if err != nil {
		return nil, xerrors.Errorf("failed to make actor code query function: %w", err)
	}

	out := make([]*lens.MessageExecution, len(executions))
	for idx, execution := range executions {
		toCode, found := getActorCode(execution.Msg.To)
		// if the message failed to execute due to lack of gas then the TO actor may never have been created.
		if !found {
			log.Warnw("failed to find TO actor", "height", next.Height().String(), "message", execution.Msg.Cid().String(), "actor", execution.Msg.To.String())
		}
		// if the message sender cannot be found this is an unexpected error
		fromCode, found := getActorCode(execution.Msg.From)
		if !found {
			return nil, xerrors.Errorf("failed to find from actor %s height %d message %s", execution.Msg.From, execution.TipSet.Height(), execution.Msg.Cid())
		}
		out[idx] = &lens.MessageExecution{
			Cid:           execution.Mcid,
			StateRoot:     execution.TipSet.ParentState(),
			Height:        execution.TipSet.Height(),
			Message:       execution.Msg,
			Ret:           execution.Ret,
			Implicit:      execution.Implicit,
			ToActorCode:   toCode,
			FromActorCode: fromCode,
		}
	}
	return out, nil
}

func (m *LilyNodeAPI) Store() adt.Store {
	m.actorStoreInit.Do(func() {
		if m.CacheConfig.StatestoreCacheSize > 0 {
			var err error
			log.Infof("creating caching statestore with size=%d", m.CacheConfig.StatestoreCacheSize)
			m.actorStore, err = util.NewCachingStateStore(m.ChainAPI.Chain.StateBlockstore(), int(m.CacheConfig.StatestoreCacheSize))
			if err == nil {
				return // done
			}

			log.Errorf("failed to create caching statestore: %v", err)
		}

		m.actorStore = m.ChainAPI.Chain.ActorStore(context.TODO())
	})

	return m.actorStore
}

func (m *LilyNodeAPI) StateGetReceipt(ctx context.Context, msg cid.Cid, from types.TipSetKey) (*types.MessageReceipt, error) {
	ml, err := m.StateSearchMsg(ctx, from, msg, api.LookbackNoLimit, true)
	if err != nil {
		return nil, err
	}

	if ml == nil {
		return nil, nil
	}

	return &ml.Receipt, nil
}

func (m *LilyNodeAPI) LogList(ctx context.Context) ([]string, error) {
	return logging.GetSubsystems(), nil
}

func (m *LilyNodeAPI) LogSetLevel(ctx context.Context, subsystem, level string) error {
	return logging.SetLogLevel(subsystem, level)
}

func (m *LilyNodeAPI) LogSetLevelRegex(ctx context.Context, regex, level string) error {
	return logging.SetLogLevelRegex(regex, level)
}

func (m *LilyNodeAPI) Shutdown(ctx context.Context) error {
	m.ShutdownChan <- struct{}{}
	return nil
}

func (m *LilyNodeAPI) LilySurvey(_ context.Context, cfg *LilySurveyConfig) (*schedule.JobSubmitResult, error) {
	// the context's passed to these methods live for the duration of the clients request, so make a new one.
	ctx := context.Background()

	// create a database connection for this watch, ensure its pingable, and run migrations if needed/configured to.
	strg, err := m.StorageCatalog.Connect(ctx, cfg.Storage, storage.Metadata{JobName: cfg.Name})
	if err != nil {
		return nil, err
	}

	// instantiate a new surveyer.
	surv, err := network.NewSurveyer(m, strg, cfg.Interval, cfg.Name, cfg.Tasks)
	if err != nil {
		return nil, err
	}

	res := m.Scheduler.Submit(&schedule.JobConfig{
		Name:  cfg.Name,
		Tasks: cfg.Tasks,
		Job:   surv,
		Params: map[string]string{
			"interval": cfg.Interval.String(),
		},
		RestartOnFailure:    cfg.RestartOnFailure,
		RestartOnCompletion: cfg.RestartOnCompletion,
		RestartDelay:        cfg.RestartDelay,
	})

	return res, nil
}

// used for debugging querries, call ORM.AddHook and this will print all queries.
type LogQueryHook struct{}

func (l *LogQueryHook) BeforeQuery(ctx context.Context, evt *pg.QueryEvent) (context.Context, error) {
	q, err := evt.FormattedQuery()
	if err != nil {
		return nil, err
	}

	if evt.Err != nil {
		fmt.Printf("%s executing a query:\n%s\n", evt.Err, q)
	}

	fmt.Println(string(q))

	return ctx, nil
}

func (l *LogQueryHook) AfterQuery(ctx context.Context, event *pg.QueryEvent) error {
	return nil
}
