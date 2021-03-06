package node

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"

	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	bcv0 "github.com/tendermint/tendermint/blockchain/v0"
	bcv1 "github.com/tendermint/tendermint/blockchain/v1"
	cfg "github.com/tendermint/tendermint/config"
	cs "github.com/tendermint/tendermint/consensus"
	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/evidence"
	cmn "github.com/tendermint/tendermint/libs/common"
	"github.com/tendermint/tendermint/libs/log"
	tmpubsub "github.com/tendermint/tendermint/libs/pubsub"
	mempl "github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/p2p/pex"
	"github.com/tendermint/tendermint/privval"
	"github.com/tendermint/tendermint/proxy"
	rpccore "github.com/tendermint/tendermint/rpc/core"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	grpccore "github.com/tendermint/tendermint/rpc/grpc"
	rpcserver "github.com/tendermint/tendermint/rpc/lib/server"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/state/txindex"
	"github.com/tendermint/tendermint/state/txindex/kv"
	"github.com/tendermint/tendermint/state/txindex/null"
	"github.com/tendermint/tendermint/store"
	"github.com/tendermint/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"
	"github.com/tendermint/tendermint/version"
	dbm "github.com/tendermint/tm-db"
)

//------------------------------------------------------------------------------

// DBContext specifies config information for loading a new DB.
type DBContext struct {
	ID     string
	Config *cfg.Config
}

// DBProvider takes a DBContext and returns an instantiated DB.
type DBProvider func(*DBContext) (dbm.DB, error)

// DefaultDBProvider returns a database using the DBBackend and DBDir
// specified in the ctx.Config.
func DefaultDBProvider(ctx *DBContext) (dbm.DB, error) {
	dbType := dbm.BackendType(ctx.Config.DBBackend)
	return dbm.NewDB(ctx.ID, dbType, ctx.Config.DBDir()), nil
}

// GenesisDocProvider returns a GenesisDoc.
// It allows the GenesisDoc to be pulled from sources other than the
// filesystem, for instance from a distributed key-value store cluster.
type GenesisDocProvider func() (*types.GenesisDoc, error)

// DefaultGenesisDocProviderFunc returns a GenesisDocProvider that loads
// the GenesisDoc from the config.GenesisFile() on the filesystem.
func DefaultGenesisDocProviderFunc(config *cfg.Config) GenesisDocProvider {
	return func() (*types.GenesisDoc, error) {
		return types.GenesisDocFromFile(config.GenesisFile())
	}
}

// NodeProvider takes a config and a logger and returns a ready to go Node.
type NodeProvider func(*cfg.Config, log.Logger) (*Node, error)

// DefaultNewNode returns a Tendermint node with default settings for the
// PrivValidator, ClientCreator, GenesisDoc, and DBProvider.
// It implements NodeProvider.
func DefaultNewNode(config *cfg.Config, logger log.Logger) (*Node, error) {
	// Generate node PrivKey
	nodeKey, err := p2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		return nil, err
	}

	// Convert old PrivValidator if it exists.
	oldPrivVal := config.OldPrivValidatorFile()
	newPrivValKey := config.PrivValidatorKeyFile()
	newPrivValState := config.PrivValidatorStateFile()
	if _, err := os.Stat(oldPrivVal); !os.IsNotExist(err) {
		oldPV, err := privval.LoadOldFilePV(oldPrivVal)
		if err != nil {
			return nil, fmt.Errorf("error reading OldPrivValidator from %v: %v\n", oldPrivVal, err)
		}
		logger.Info("Upgrading PrivValidator file",
			"old", oldPrivVal,
			"newKey", newPrivValKey,
			"newState", newPrivValState,
		)
		oldPV.Upgrade(newPrivValKey, newPrivValState)
	}

	return NewNode(config,
		privval.LoadOrGenFilePV(newPrivValKey, newPrivValState),
		nodeKey,
		proxy.DefaultClientCreator(config.ProxyApp, config.ABCI, config.DBDir()),
		DefaultGenesisDocProviderFunc(config),
		DefaultDBProvider,
		DefaultMetricsProvider(config.Instrumentation),
		logger,
	)
}

// MetricsProvider returns a consensus, p2p and mempool Metrics.
type MetricsProvider func(chainID string) (*cs.Metrics, *p2p.Metrics, *mempl.Metrics, *sm.Metrics)

// DefaultMetricsProvider returns Metrics build using Prometheus client library
// if Prometheus is enabled. Otherwise, it returns no-op Metrics.
func DefaultMetricsProvider(config *cfg.InstrumentationConfig) MetricsProvider {
	return func(chainID string) (*cs.Metrics, *p2p.Metrics, *mempl.Metrics, *sm.Metrics) {
		if config.Prometheus {
			return cs.PrometheusMetrics(config.Namespace, "chain_id", chainID),
				p2p.PrometheusMetrics(config.Namespace, "chain_id", chainID),
				mempl.PrometheusMetrics(config.Namespace, "chain_id", chainID),
				sm.PrometheusMetrics(config.Namespace, "chain_id", chainID)
		}
		return cs.NopMetrics(), p2p.NopMetrics(), mempl.NopMetrics(), sm.NopMetrics()
	}
}

// Option sets a parameter for the node.
type Option func(*Node)

// CustomReactors allows you to add custom reactors (name -> p2p.Reactor) to
// the node's Switch.
//
// WARNING: using any name from the below list of the existing reactors will
// result in replacing it with the custom one.
//
//  - MEMPOOL
//  - BLOCKCHAIN
//  - CONSENSUS
//  - EVIDENCE
//  - PEX
func CustomReactors(reactors map[string]p2p.Reactor) Option {
	return func(n *Node) {
		for name, reactor := range reactors {
			if existingReactor := n.Sw.Reactor(name); existingReactor != nil {
				n.Sw.Logger.Info("Replacing existing reactor with a custom one",
					"name", name, "existing", existingReactor, "custom", reactor)
				n.Sw.RemoveReactor(name, existingReactor)
			}
			n.Sw.AddReactor(name, reactor)
		}
	}
}

func CustomConsensusState(state *cs.ConsensusState) Option {
	return func(n *Node) {
		n.ConsensusState = state
	}
}

//------------------------------------------------------------------------------

// Node is the highest level interface to a full Tendermint node.
// It includes all configuration information and running services.
type Node struct {
	cmn.BaseService

	// config
	Config        *cfg.Config
	GenesisDoc    *types.GenesisDoc   // initial validator set
	PrivValidator types.PrivValidator // local node's validator key

	// network
	Transport   *p2p.MultiplexTransport
	Sw          *p2p.Switch  // p2p connections
	AddrBook    pex.AddrBook // known peers
	NodeInfo    p2p.NodeInfo
	NodeKey     *p2p.NodeKey // our node privkey
	IsListening bool

	// services
	EventBus         *types.EventBus // pub/sub for services
	StateDB          dbm.DB
	BlockStore       *store.BlockStore // store the blockchain to disk
	BcReactor        p2p.Reactor       // for fast-syncing
	MempoolReactor   *mempl.Reactor    // for gossipping transactions
	Mempool          mempl.Mempool
	ConsensusState   *cs.ConsensusState                // latest consensus state
	ConsensusReactor rpccore.ConsensusReactorInterface // for participating in the consensus
	PexReactor       *pex.PEXReactor                   // for exchanging peer addresses
	EvidencePool     *evidence.EvidencePool            // tracking evidence
	ProxyApp         proxy.AppConns                    // connection to the application
	RpcListeners     []net.Listener                    // rpc servers
	TxIndexer        txindex.TxIndexer
	IndexerService   *txindex.IndexerService
	PrometheusSrv    *http.Server
}

func InitDBs(config *cfg.Config, dbProvider DBProvider) (blockStore *store.BlockStore, stateDB dbm.DB, err error) {
	var blockStoreDB dbm.DB
	blockStoreDB, err = dbProvider(&DBContext{"blockstore", config})
	if err != nil {
		return
	}
	blockStore = store.NewBlockStore(blockStoreDB)

	stateDB, err = dbProvider(&DBContext{"state", config})
	if err != nil {
		return
	}

	return
}

func CreateAndStartProxyAppConns(clientCreator proxy.ClientCreator, logger log.Logger) (proxy.AppConns, error) {
	proxyApp := proxy.NewAppConns(clientCreator)
	proxyApp.SetLogger(logger.With("module", "proxy"))
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error starting proxy app connections: %v", err)
	}
	return proxyApp, nil
}

func CreateAndStartEventBus(logger log.Logger) (*types.EventBus, error) {
	eventBus := types.NewEventBus()
	eventBus.SetLogger(logger.With("module", "events"))
	if err := eventBus.Start(); err != nil {
		return nil, err
	}
	return eventBus, nil
}

func CreateAndStartIndexerService(config *cfg.Config, dbProvider DBProvider,
	eventBus *types.EventBus, logger log.Logger) (*txindex.IndexerService, txindex.TxIndexer, error) {

	var txIndexer txindex.TxIndexer
	switch config.TxIndex.Indexer {
	case "kv":
		store, err := dbProvider(&DBContext{"tx_index", config})
		if err != nil {
			return nil, nil, err
		}
		switch {
		case config.TxIndex.IndexTags != "":
			txIndexer = kv.NewTxIndex(store, kv.IndexTags(SplitAndTrimEmpty(config.TxIndex.IndexTags, ",", " ")))
		case config.TxIndex.IndexAllTags:
			txIndexer = kv.NewTxIndex(store, kv.IndexAllTags())
		default:
			txIndexer = kv.NewTxIndex(store)
		}
	default:
		txIndexer = &null.TxIndex{}
	}

	indexerService := txindex.NewIndexerService(txIndexer, eventBus)
	indexerService.SetLogger(logger.With("module", "txindex"))
	if err := indexerService.Start(); err != nil {
		return nil, nil, err
	}
	return indexerService, txIndexer, nil
}

func DoHandshake(
	stateDB dbm.DB,
	state sm.State,
	blockStore sm.BlockStore,
	genDoc *types.GenesisDoc,
	eventBus types.BlockEventPublisher,
	proxyApp proxy.AppConns,
	consensusLogger log.Logger) error {

	handshaker := cs.NewHandshaker(stateDB, state, blockStore, genDoc)
	handshaker.SetLogger(consensusLogger)
	handshaker.SetEventBus(eventBus)
	if err := handshaker.Handshake(proxyApp); err != nil {
		return fmt.Errorf("error during handshake: %v", err)
	}
	return nil
}

func logNodeStartupInfo(state sm.State, pubKey crypto.PubKey, logger, consensusLogger log.Logger) {
	// Log the version info.
	logger.Info("Version info",
		"software", version.TMCoreSemVer,
		"block", version.BlockProtocol,
		"p2p", version.P2PProtocol,
	)

	// If the state and software differ in block version, at least log it.
	if state.Version.Consensus.Block != version.BlockProtocol {
		logger.Info("Software and state have different block protocols",
			"software", version.BlockProtocol,
			"state", state.Version.Consensus.Block,
		)
	}

	addr := pubKey.Address()
	// Log whether this node is a validator or an observer
	if state.Validators.HasAddress(addr) {
		consensusLogger.Info("This node is a validator", "addr", addr, "pubKey", pubKey)
	} else {
		consensusLogger.Info("This node is not a validator", "addr", addr, "pubKey", pubKey)
	}
}

func OnlyValidatorIsUs(state sm.State, privVal types.PrivValidator) bool {
	if state.Validators.Size() > 1 {
		return false
	}
	addr, _ := state.Validators.GetByIndex(0)
	return bytes.Equal(privVal.GetPubKey().Address(), addr)
}

func CreateMempoolAndMempoolReactor(config *cfg.Config, proxyApp proxy.AppConns,
	state sm.State, memplMetrics *mempl.Metrics, logger log.Logger) (*mempl.Reactor, *mempl.CListMempool) {

	mempool := mempl.NewCListMempool(
		config.Mempool,
		proxyApp.Mempool(),
		state.LastBlockHeight,
		mempl.WithMetrics(memplMetrics),
		mempl.WithPreCheck(sm.TxPreCheck(state)),
		mempl.WithPostCheck(sm.TxPostCheck(state)),
	)
	mempoolLogger := logger.With("module", "mempool")
	mempoolReactor := mempl.NewReactor(config.Mempool, mempool)
	mempoolReactor.SetLogger(mempoolLogger)

	if config.Consensus.WaitForTxs() {
		mempool.EnableTxsAvailable()
	}
	return mempoolReactor, mempool
}

func CreateEvidenceReactor(config *cfg.Config, dbProvider DBProvider,
	stateDB dbm.DB, logger log.Logger) (*evidence.EvidenceReactor, *evidence.EvidencePool, error) {

	evidenceDB, err := dbProvider(&DBContext{"evidence", config})
	if err != nil {
		return nil, nil, err
	}
	evidenceLogger := logger.With("module", "evidence")
	evidencePool := evidence.NewEvidencePool(stateDB, evidenceDB)
	evidencePool.SetLogger(evidenceLogger)
	evidenceReactor := evidence.NewEvidenceReactor(evidencePool)
	evidenceReactor.SetLogger(evidenceLogger)
	return evidenceReactor, evidencePool, nil
}

func createBlockchainReactor(config *cfg.Config,
	state sm.State,
	blockExec *sm.BlockExecutor,
	blockStore *store.BlockStore,
	fastSync bool,
	logger log.Logger) (bcReactor p2p.Reactor, err error) {

	switch config.FastSync.Version {
	case "v0":
		bcReactor = bcv0.NewBlockchainReactor(state.Copy(), blockExec, blockStore, fastSync)
	case "v1":
		bcReactor = bcv1.NewBlockchainReactor(state.Copy(), blockExec, blockStore, fastSync)
	default:
		return nil, fmt.Errorf("unknown fastsync version %s", config.FastSync.Version)
	}

	bcReactor.SetLogger(logger.With("module", "blockchain"))
	return bcReactor, nil
}

func createConsensusReactor(config *cfg.Config,
	state sm.State,
	blockExec *sm.BlockExecutor,
	blockStore sm.BlockStore,
	mempool *mempl.CListMempool,
	evidencePool *evidence.EvidencePool,
	privValidator types.PrivValidator,
	csMetrics *cs.Metrics,
	fastSync bool,
	eventBus *types.EventBus,
	consensusLogger log.Logger) (*cs.ConsensusReactor, *cs.ConsensusState) {

	consensusState := cs.NewConsensusState(
		config.Consensus,
		state.Copy(),
		blockExec,
		blockStore,
		mempool,
		evidencePool,
		cs.StateMetrics(csMetrics),
	)
	consensusState.SetLogger(consensusLogger)
	if privValidator != nil {
		consensusState.SetPrivValidator(privValidator)
	}
	consensusReactor := cs.NewConsensusReactor(consensusState, fastSync, cs.ReactorMetrics(csMetrics))
	consensusReactor.SetLogger(consensusLogger)
	// services which will be publishing and/or subscribing for messages (events)
	// consensusReactor will set it on consensusState and blockExecutor
	consensusReactor.SetEventBus(eventBus)
	return consensusReactor, consensusState
}

func CreateTransport(
	config *cfg.Config,
	nodeInfo p2p.NodeInfo,
	nodeKey *p2p.NodeKey,
	proxyApp proxy.AppConns,
) (
	*p2p.MultiplexTransport,
	[]p2p.PeerFilterFunc,
) {
	var (
		mConnConfig = p2p.MConnConfig(config.P2P)
		transport   = p2p.NewMultiplexTransport(nodeInfo, *nodeKey, mConnConfig)
		connFilters = []p2p.ConnFilterFunc{}
		peerFilters = []p2p.PeerFilterFunc{}
	)

	if !config.P2P.AllowDuplicateIP {
		connFilters = append(connFilters, p2p.ConnDuplicateIPFilter())
	}

	// Filter peers by addr or pubkey with an ABCI query.
	// If the query return code is OK, add peer.
	if config.FilterPeers {
		connFilters = append(
			connFilters,
			// ABCI query for address filtering.
			func(_ p2p.ConnSet, c net.Conn, _ []net.IP) error {
				res, err := proxyApp.Query().QuerySync(abci.RequestQuery{
					Path: fmt.Sprintf("/p2p/filter/addr/%s", c.RemoteAddr().String()),
				})
				if err != nil {
					return err
				}
				if res.IsErr() {
					return fmt.Errorf("error querying abci app: %v", res)
				}

				return nil
			},
		)

		peerFilters = append(
			peerFilters,
			// ABCI query for ID filtering.
			func(_ p2p.IPeerSet, p p2p.Peer) error {
				res, err := proxyApp.Query().QuerySync(abci.RequestQuery{
					Path: fmt.Sprintf("/p2p/filter/id/%s", p.ID()),
				})
				if err != nil {
					return err
				}
				if res.IsErr() {
					return fmt.Errorf("error querying abci app: %v", res)
				}

				return nil
			},
		)
	}

	p2p.MultiplexTransportConnFilters(connFilters...)(transport)
	return transport, peerFilters
}

func createSwitch(config *cfg.Config,
	transport p2p.Transport,
	p2pMetrics *p2p.Metrics,
	peerFilters []p2p.PeerFilterFunc,
	mempoolReactor *mempl.Reactor,
	bcReactor p2p.Reactor,
	consensusReactor *cs.ConsensusReactor,
	evidenceReactor *evidence.EvidenceReactor,
	nodeInfo p2p.NodeInfo,
	nodeKey *p2p.NodeKey,
	p2pLogger log.Logger) *p2p.Switch {

	sw := p2p.NewSwitch(
		config.P2P,
		transport,
		p2p.WithMetrics(p2pMetrics),
		p2p.SwitchPeerFilters(peerFilters...),
	)
	sw.SetLogger(p2pLogger)
	sw.AddReactor("MEMPOOL", mempoolReactor)
	sw.AddReactor("BLOCKCHAIN", bcReactor)
	sw.AddReactor("CONSENSUS", consensusReactor)
	sw.AddReactor("EVIDENCE", evidenceReactor)

	sw.SetNodeInfo(nodeInfo)
	sw.SetNodeKey(nodeKey)

	p2pLogger.Info("P2P Node ID", "ID", nodeKey.ID(), "file", config.NodeKeyFile())
	return sw
}

func CreateAddrBookAndSetOnSwitch(config *cfg.Config, sw *p2p.Switch,
	p2pLogger log.Logger, nodeKey *p2p.NodeKey) (pex.AddrBook, error) {

	addrBook := pex.NewAddrBook(config.P2P.AddrBookFile(), config.P2P.AddrBookStrict)
	addrBook.SetLogger(p2pLogger.With("book", config.P2P.AddrBookFile()))

	// Add ourselves to addrbook to prevent dialing ourselves
	if config.P2P.ExternalAddress != "" {
		addr, err := p2p.NewNetAddressString(p2p.IDAddressString(nodeKey.ID(), config.P2P.ExternalAddress))
		if err != nil {
			return nil, errors.Wrap(err, "p2p.external_address is incorrect")
		}
		addrBook.AddOurAddress(addr)
	}
	if config.P2P.ListenAddress != "" {
		addr, err := p2p.NewNetAddressString(p2p.IDAddressString(nodeKey.ID(), config.P2P.ListenAddress))
		if err != nil {
			return nil, errors.Wrap(err, "p2p.laddr is incorrect")
		}
		addrBook.AddOurAddress(addr)
	}

	sw.SetAddrBook(addrBook)

	return addrBook, nil
}

func CreatePEXReactorAndAddToSwitch(addrBook pex.AddrBook, config *cfg.Config,
	sw *p2p.Switch, logger log.Logger) *pex.PEXReactor {

	// TODO persistent peers ? so we can have their DNS addrs saved
	pexReactor := pex.NewPEXReactor(addrBook,
		&pex.PEXReactorConfig{
			Seeds:    SplitAndTrimEmpty(config.P2P.Seeds, ",", " "),
			SeedMode: config.P2P.SeedMode,
			// See consensus/reactor.go: blocksToContributeToBecomeGoodPeer 10000
			// blocks assuming 10s blocks ~ 28 hours.
			// TODO (melekes): make it dynamic based on the actual block latencies
			// from the live network.
			// https://github.com/tendermint/tendermint/issues/3523
			SeedDisconnectWaitPeriod: 28 * time.Hour,
		})
	pexReactor.SetLogger(logger.With("module", "pex"))
	sw.AddReactor("PEX", pexReactor)
	return pexReactor
}

// NewNode returns a new, ready to go, Tendermint Node.
func NewNode(config *cfg.Config,
	privValidator types.PrivValidator,
	nodeKey *p2p.NodeKey,
	clientCreator proxy.ClientCreator,
	genesisDocProvider GenesisDocProvider,
	dbProvider DBProvider,
	metricsProvider MetricsProvider,
	logger log.Logger,
	options ...Option) (*Node, error) {

	blockStore, stateDB, err := InitDBs(config, dbProvider)
	if err != nil {
		return nil, err
	}

	state, genDoc, err := LoadStateFromDBOrGenesisDocProvider(stateDB, genesisDocProvider)
	if err != nil {
		return nil, err
	}

	logger.Debug("state creation", "validators", state.Validators)

	// Create the proxyApp and establish connections to the ABCI app (consensus, mempool, query).
	proxyApp, err := CreateAndStartProxyAppConns(clientCreator, logger)
	if err != nil {
		return nil, err
	}

	// EventBus and IndexerService must be started before the handshake because
	// we might need to index the txs of the replayed block as this might not have happened
	// when the node stopped last time (i.e. the node stopped after it saved the block
	// but before it indexed the txs, or, endblocker panicked)
	eventBus, err := CreateAndStartEventBus(logger)
	if err != nil {
		return nil, err
	}

	// Transaction indexing
	indexerService, txIndexer, err := CreateAndStartIndexerService(config, dbProvider, eventBus, logger)
	if err != nil {
		return nil, err
	}

	// Create the handshaker, which calls RequestInfo, sets the AppVersion on the state,
	// and replays any blocks as necessary to sync tendermint with the app.
	consensusLogger := logger.With("module", "consensus")
	if err := DoHandshake(stateDB, state, blockStore, genDoc, eventBus, proxyApp, consensusLogger); err != nil {
		return nil, err
	}

	logger.Debug("state do handshake", "validators", state.Validators)

	// Reload the state. It will have the Version.Consensus.App set by the
	// Handshake, and may have other modifications as well (ie. depending on
	// what happened during block replay).
	state = sm.LoadState(stateDB)
	logger.Debug("state load state", "validators", state.Validators)

	// If an address is provided, listen on the socket for a connection from an
	// external signing process.
	if config.PrivValidatorListenAddr != "" {
		// FIXME: we should start services inside OnStart
		privValidator, err = CreateAndStartPrivValidatorSocketClient(config.PrivValidatorListenAddr, logger)
		if err != nil {
			return nil, errors.Wrap(err, "error with private validator socket client")
		}
	}

	pubKey := privValidator.GetPubKey()
	if pubKey == nil {
		// TODO: GetPubKey should return errors - https://github.com/tendermint/tendermint/issues/3602
		return nil, errors.New("could not retrieve public key from private validator")
	}

	logNodeStartupInfo(state, pubKey, logger, consensusLogger)
	logger.Debug("state log node startup info", "validators", state.Validators)

	// Decide whether to fast-sync or not
	// We don't fast-sync when the only validator is us.
	fastSync := config.FastSyncMode && !OnlyValidatorIsUs(state, privValidator)

	csMetrics, p2pMetrics, memplMetrics, smMetrics := metricsProvider(genDoc.ChainID)

	// Make MempoolReactor
	mempoolReactor, mempool := CreateMempoolAndMempoolReactor(config, proxyApp, state, memplMetrics, logger)

	logger.Debug("state create mempool", "validators", state.Validators)

	// Make Evidence Reactor
	evidenceReactor, evidencePool, err := CreateEvidenceReactor(config, dbProvider, stateDB, logger)
	if err != nil {
		return nil, err
	}

	// make block executor for consensus and blockchain reactors to execute blocks
	blockExec := sm.NewBlockExecutor(
		stateDB,
		logger.With("module", "state"),
		proxyApp.Consensus(),
		mempool,
		evidencePool,
		sm.BlockExecutorWithMetrics(smMetrics),
	)

	// Make BlockchainReactor
	bcReactor, err := createBlockchainReactor(config, state, blockExec, blockStore, fastSync, logger)
	if err != nil {
		return nil, errors.Wrap(err, "could not create blockchain reactor")
	}

	// Make ConsensusReactor
	consensusReactor, consensusState := createConsensusReactor(
		config, state, blockExec, blockStore, mempool, evidencePool,
		privValidator, csMetrics, fastSync, eventBus, consensusLogger,
	)
	logger.Debug("state create consensus reactors", "validators", state.Validators)

	nodeInfo, err := MakeNodeInfo(config, nodeKey, txIndexer, genDoc, state)
	if err != nil {
		return nil, err
	}
	logger.Debug("state make node info", "validators", state.Validators)

	// Setup Transport.
	transport, peerFilters := CreateTransport(config, nodeInfo, nodeKey, proxyApp)

	// Setup Switch.
	p2pLogger := logger.With("module", "p2p")
	sw := createSwitch(
		config, transport, p2pMetrics, peerFilters, mempoolReactor, bcReactor,
		consensusReactor, evidenceReactor, nodeInfo, nodeKey, p2pLogger,
	)

	err = sw.AddPersistentPeers(SplitAndTrimEmpty(config.P2P.PersistentPeers, ",", " "))
	if err != nil {
		return nil, errors.Wrap(err, "could not add peers from persistent_peers field")
	}

	addrBook, err := CreateAddrBookAndSetOnSwitch(config, sw, p2pLogger, nodeKey)
	if err != nil {
		return nil, errors.Wrap(err, "could not create addrbook")
	}

	// Optionally, start the pex reactor
	//
	// TODO:
	//
	// We need to set Seeds and PersistentPeers on the switch,
	// since it needs to be able to use these (and their DNS names)
	// even if the PEX is off. We can include the DNS name in the NetAddress,
	// but it would still be nice to have a clear list of the current "PersistentPeers"
	// somewhere that we can return with net_info.
	//
	// If PEX is on, it should handle dialing the seeds. Otherwise the switch does it.
	// Note we currently use the addrBook regardless at least for AddOurAddress
	var pexReactor *pex.PEXReactor
	if config.P2P.PexReactor {
		pexReactor = CreatePEXReactorAndAddToSwitch(addrBook, config, sw, logger)
	}

	if config.ProfListenAddress != "" {
		go func() {
			logger.Error("Profile server", "err", http.ListenAndServe(config.ProfListenAddress, nil))
		}()
	}

	node := &Node{
		Config:        config,
		GenesisDoc:    genDoc,
		PrivValidator: privValidator,

		Transport: transport,
		Sw:        sw,
		AddrBook:  addrBook,
		NodeInfo:  nodeInfo,
		NodeKey:   nodeKey,

		StateDB:          stateDB,
		BlockStore:       blockStore,
		BcReactor:        bcReactor,
		MempoolReactor:   mempoolReactor,
		Mempool:          mempool,
		ConsensusState:   consensusState,
		ConsensusReactor: consensusReactor,
		PexReactor:       pexReactor,
		EvidencePool:     evidencePool,
		ProxyApp:         proxyApp,
		TxIndexer:        txIndexer,
		IndexerService:   indexerService,
		EventBus:         eventBus,
	}
	node.BaseService = *cmn.NewBaseService(logger, "Node", node)

	for _, option := range options {
		option(node)
	}

	return node, nil
}

// OnStart starts the Node. It implements cmn.Service.
func (n *Node) OnStart() error {
	now := tmtime.Now()
	genTime := n.GenesisDoc.GenesisTime
	if genTime.After(now) {
		n.Logger.Info("Genesis time is in the future. Sleeping until then...", "genTime", genTime)
		time.Sleep(genTime.Sub(now))
	}

	// Add private IDs to addrbook to block those peers being added
	n.AddrBook.AddPrivateIDs(SplitAndTrimEmpty(n.Config.P2P.PrivatePeerIDs, ",", " "))

	// Start the RPC server before the P2P server
	// so we can eg. receive txs for the first block
	if n.Config.RPC.ListenAddress != "" {
		listeners, err := n.startRPC()
		if err != nil {
			return err
		}
		n.RpcListeners = listeners
	}

	if n.Config.Instrumentation.Prometheus &&
		n.Config.Instrumentation.PrometheusListenAddr != "" {
		n.PrometheusSrv = n.startPrometheusServer(n.Config.Instrumentation.PrometheusListenAddr)
	}

	// Start the transport.
	addr, err := p2p.NewNetAddressString(p2p.IDAddressString(n.NodeKey.ID(), n.Config.P2P.ListenAddress))
	if err != nil {
		return err
	}
	if err := n.Transport.Listen(*addr); err != nil {
		return err
	}

	n.IsListening = true

	if n.Config.Mempool.WalEnabled() {
		n.Mempool.InitWAL() // no need to have the mempool wal during tests
	}

	// Start the switch (the P2P server).
	err = n.Sw.Start()
	if err != nil {
		return err
	}

	// Always connect to persistent peers
	err = n.Sw.DialPeersAsync(SplitAndTrimEmpty(n.Config.P2P.PersistentPeers, ",", " "))
	if err != nil {
		return errors.Wrap(err, "could not dial peers from persistent_peers field")
	}

	return nil
}

// OnStop stops the Node. It implements cmn.Service.
func (n *Node) OnStop() {
	n.BaseService.OnStop()

	n.Logger.Info("Stopping Node")

	// first stop the non-reactor services
	n.EventBus.Stop()
	n.IndexerService.Stop()

	// now stop the reactors
	n.Sw.Stop()

	// stop mempool WAL
	if n.Config.Mempool.WalEnabled() {
		n.Mempool.CloseWAL()
	}

	if err := n.Transport.Close(); err != nil {
		n.Logger.Error("Error closing transport", "err", err)
	}

	n.IsListening = false

	// finally stop the listeners / external services
	for _, l := range n.RpcListeners {
		n.Logger.Info("Closing rpc listener", "listener", l)
		if err := l.Close(); err != nil {
			n.Logger.Error("Error closing listener", "listener", l, "err", err)
		}
	}

	if pvsc, ok := n.PrivValidator.(cmn.Service); ok {
		pvsc.Stop()
	}

	if n.PrometheusSrv != nil {
		if err := n.PrometheusSrv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			n.Logger.Error("Prometheus HTTP server Shutdown", "err", err)
		}
	}
}

// ConfigureRPC sets all variables in rpccore so they will serve
// rpc calls from this node
func (n *Node) ConfigureRPC() {
	rpccore.SetStateDB(n.StateDB)
	rpccore.SetBlockStore(n.BlockStore)
	rpccore.SetConsensusState(n.ConsensusState)
	rpccore.SetMempool(n.Mempool)
	rpccore.SetEvidencePool(n.EvidencePool)
	rpccore.SetP2PPeers(n.Sw)
	rpccore.SetP2PTransport(n)
	pubKey := n.PrivValidator.GetPubKey()
	rpccore.SetPubKey(pubKey)
	rpccore.SetGenesisDoc(n.GenesisDoc)
	rpccore.SetProxyAppQuery(n.ProxyApp.Query())
	rpccore.SetTxIndexer(n.TxIndexer)
	rpccore.SetConsensusReactor(n.ConsensusReactor)
	rpccore.SetEventBus(n.EventBus)
	rpccore.SetLogger(n.Logger.With("module", "rpc"))
	rpccore.SetConfig(*n.Config.RPC)
}

func (n *Node) startRPC() ([]net.Listener, error) {
	n.ConfigureRPC()
	listenAddrs := SplitAndTrimEmpty(n.Config.RPC.ListenAddress, ",", " ")
	coreCodec := amino.NewCodec()
	ctypes.RegisterAmino(coreCodec)

	if n.Config.RPC.Unsafe {
		rpccore.AddUnsafeRoutes()
	}

	config := rpcserver.DefaultConfig()
	config.MaxBodyBytes = n.Config.RPC.MaxBodyBytes
	config.MaxHeaderBytes = n.Config.RPC.MaxHeaderBytes
	config.MaxOpenConnections = n.Config.RPC.MaxOpenConnections
	// If necessary adjust global WriteTimeout to ensure it's greater than
	// TimeoutBroadcastTxCommit.
	// See https://github.com/tendermint/tendermint/issues/3435
	if config.WriteTimeout <= n.Config.RPC.TimeoutBroadcastTxCommit {
		config.WriteTimeout = n.Config.RPC.TimeoutBroadcastTxCommit + 1*time.Second
	}

	// we may expose the rpc over both a unix and tcp socket
	listeners := make([]net.Listener, len(listenAddrs))
	for i, listenAddr := range listenAddrs {
		mux := http.NewServeMux()
		rpcLogger := n.Logger.With("module", "rpc-server")
		wmLogger := rpcLogger.With("protocol", "websocket")
		wm := rpcserver.NewWebsocketManager(rpccore.Routes, coreCodec,
			rpcserver.OnDisconnect(func(remoteAddr string) {
				err := n.EventBus.UnsubscribeAll(context.Background(), remoteAddr)
				if err != nil && err != tmpubsub.ErrSubscriptionNotFound {
					wmLogger.Error("Failed to unsubscribe addr from events", "addr", remoteAddr, "err", err)
				}
			}),
			rpcserver.ReadLimit(config.MaxBodyBytes),
		)
		wm.SetLogger(wmLogger)
		mux.HandleFunc("/websocket", wm.WebsocketHandler)
		rpcserver.RegisterRPCFuncs(mux, rpccore.Routes, coreCodec, rpcLogger)
		listener, err := rpcserver.Listen(
			listenAddr,
			config,
		)
		if err != nil {
			return nil, err
		}

		var rootHandler http.Handler = mux
		if n.Config.RPC.IsCorsEnabled() {
			corsMiddleware := cors.New(cors.Options{
				AllowedOrigins: n.Config.RPC.CORSAllowedOrigins,
				AllowedMethods: n.Config.RPC.CORSAllowedMethods,
				AllowedHeaders: n.Config.RPC.CORSAllowedHeaders,
			})
			rootHandler = corsMiddleware.Handler(mux)
		}
		if n.Config.RPC.IsTLSEnabled() {
			go rpcserver.StartHTTPAndTLSServer(
				listener,
				rootHandler,
				n.Config.RPC.CertFile(),
				n.Config.RPC.KeyFile(),
				rpcLogger,
				config,
			)
		} else {
			go rpcserver.StartHTTPServer(
				listener,
				rootHandler,
				rpcLogger,
				config,
			)
		}

		listeners[i] = listener
	}

	// we expose a simplified api over grpc for convenience to app devs
	grpcListenAddr := n.Config.RPC.GRPCListenAddress
	if grpcListenAddr != "" {
		config := rpcserver.DefaultConfig()
		config.MaxOpenConnections = n.Config.RPC.MaxOpenConnections
		listener, err := rpcserver.Listen(grpcListenAddr, config)
		if err != nil {
			return nil, err
		}
		go grpccore.StartGRPCServer(listener)
		listeners = append(listeners, listener)
	}

	return listeners, nil
}

// startPrometheusServer starts a Prometheus HTTP server, listening for metrics
// collectors on addr.
func (n *Node) startPrometheusServer(addr string) *http.Server {
	srv := &http.Server{
		Addr: addr,
		Handler: promhttp.InstrumentMetricHandler(
			prometheus.DefaultRegisterer, promhttp.HandlerFor(
				prometheus.DefaultGatherer,
				promhttp.HandlerOpts{MaxRequestsInFlight: n.Config.Instrumentation.MaxOpenConnections},
			),
		),
	}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			// Error starting or closing listener:
			n.Logger.Error("Prometheus HTTP server ListenAndServe", "err", err)
		}
	}()
	return srv
}

// Switch returns the Node's Switch.
func (n *Node) Switch() *p2p.Switch {
	return n.Sw
}

// BlockStore returns the Node's BlockStore.
func (n *Node) GetBlockStore() *store.BlockStore {
	return n.BlockStore
}

// ConsensusState returns the Node's ConsensusState.
func (n *Node) GetConsensusState() *cs.ConsensusState {
	return n.ConsensusState
}

// ConsensusReactor returns the Node's ConsensusReactor.
func (n *Node) GetConsensusReactor() rpccore.ConsensusReactorInterface {
	return n.ConsensusReactor
}

// MempoolReactor returns the Node's mempool reactor.
func (n *Node) GetMempoolReactor() *mempl.Reactor {
	return n.MempoolReactor
}

// Mempool returns the Node's mempool.
func (n *Node) GetMempool() mempl.Mempool {
	return n.Mempool
}

// PEXReactor returns the Node's PEXReactor. It returns nil if PEX is disabled.
func (n *Node) GetPEXReactor() *pex.PEXReactor {
	return n.PexReactor
}

// EvidencePool returns the Node's EvidencePool.
func (n *Node) GetEvidencePool() *evidence.EvidencePool {
	return n.EvidencePool
}

// EventBus returns the Node's EventBus.
func (n *Node) GetEventBus() *types.EventBus {
	return n.EventBus
}

// PrivValidator returns the Node's PrivValidator.
// XXX: for convenience only!
func (n *Node) GetPrivValidator() types.PrivValidator {
	return n.PrivValidator
}

// GenesisDoc returns the Node's GenesisDoc.
func (n *Node) GetGenesisDoc() *types.GenesisDoc {
	return n.GenesisDoc
}

// ProxyApp returns the Node's AppConns, representing its connections to the ABCI application.
func (n *Node) GetProxyApp() proxy.AppConns {
	return n.ProxyApp
}

// Config returns the Node's config.
func (n *Node) GetConfig() *cfg.Config {
	return n.Config
}

//------------------------------------------------------------------------------

func (n *Node) Listeners() []string {
	return []string{
		fmt.Sprintf("Listener(@%v)", n.Config.P2P.ExternalAddress),
	}
}

func (n *Node) GetIsListening() bool {
	return n.IsListening
}

// NodeInfo returns the Node's Info from the Switch.
func (n *Node) GetNodeInfo() p2p.NodeInfo {
	return n.NodeInfo
}

func MakeNodeInfo(
	config *cfg.Config,
	nodeKey *p2p.NodeKey,
	txIndexer txindex.TxIndexer,
	genDoc *types.GenesisDoc,
	state sm.State,
) (p2p.NodeInfo, error) {
	txIndexerStatus := "on"
	if _, ok := txIndexer.(*null.TxIndex); ok {
		txIndexerStatus = "off"
	}

	var bcChannel byte
	switch config.FastSync.Version {
	case "v0":
		bcChannel = bcv0.BlockchainChannel
	case "v1":
		bcChannel = bcv1.BlockchainChannel
	default:
		return nil, fmt.Errorf("unknown fastsync version %s", config.FastSync.Version)
	}

	nodeInfo := p2p.DefaultNodeInfo{
		ProtocolVersion: p2p.NewProtocolVersion(
			version.P2PProtocol, // global
			state.Version.Consensus.Block,
			state.Version.Consensus.App,
		),
		ID_:     nodeKey.ID(),
		Network: genDoc.ChainID,
		Version: version.TMCoreSemVer,
		Channels: []byte{
			bcChannel,
			cs.StateChannel, cs.DataChannel, cs.VoteChannel, cs.VoteSetBitsChannel,
			mempl.MempoolChannel,
			evidence.EvidenceChannel,
		},
		Moniker: config.Moniker,
		Other: p2p.DefaultNodeInfoOther{
			TxIndex:    txIndexerStatus,
			RPCAddress: config.RPC.ListenAddress,
		},
	}

	if config.P2P.PexReactor {
		nodeInfo.Channels = append(nodeInfo.Channels, pex.PexChannel)
	}

	lAddr := config.P2P.ExternalAddress

	if lAddr == "" {
		lAddr = config.P2P.ListenAddress
	}

	nodeInfo.ListenAddr = lAddr

	err := nodeInfo.Validate()
	return nodeInfo, err
}

//------------------------------------------------------------------------------

var (
	genesisDocKey = []byte("genesisDoc")
)

// LoadStateFromDBOrGenesisDocProvider attempts to load the state from the
// database, or creates one using the given genesisDocProvider and persists the
// result to the database. On success this also returns the genesis doc loaded
// through the given provider.
func LoadStateFromDBOrGenesisDocProvider(
	stateDB dbm.DB,
	genesisDocProvider GenesisDocProvider,
) (sm.State, *types.GenesisDoc, error) {
	// Get genesis doc
	genDoc, err := loadGenesisDoc(stateDB)
	if err != nil {
		genDoc, err = genesisDocProvider()
		if err != nil {
			return sm.State{}, nil, err
		}
		// save genesis doc to prevent a certain class of user errors (e.g. when it
		// was changed, accidentally or not). Also good for audit trail.
		saveGenesisDoc(stateDB, genDoc)
	}
	state, err := sm.LoadStateFromDBOrGenesisDoc(stateDB, genDoc)
	if err != nil {
		return sm.State{}, nil, err
	}
	return state, genDoc, nil
}

// panics if failed to unmarshal bytes
func loadGenesisDoc(db dbm.DB) (*types.GenesisDoc, error) {
	b := db.Get(genesisDocKey)
	if len(b) == 0 {
		return nil, errors.New("Genesis doc not found")
	}
	var genDoc *types.GenesisDoc
	err := Cdc.UnmarshalJSON(b, &genDoc)
	if err != nil {
		panic(fmt.Sprintf("Failed to load genesis doc due to unmarshaling error: %v (bytes: %X)", err, b))
	}
	return genDoc, nil
}

// panics if failed to marshal the given genesis document
func saveGenesisDoc(db dbm.DB, genDoc *types.GenesisDoc) {
	b, err := Cdc.MarshalJSON(genDoc)
	if err != nil {
		panic(fmt.Sprintf("Failed to save genesis doc due to marshaling error: %v", err))
	}
	db.SetSync(genesisDocKey, b)
}

func CreateAndStartPrivValidatorSocketClient(
	listenAddr string,
	logger log.Logger,
) (types.PrivValidator, error) {
	pve, err := privval.NewSignerListener(listenAddr, logger)
	if err != nil {
		return nil, errors.Wrap(err, "failed to start private validator")
	}

	pvsc, err := privval.NewSignerClient(pve)
	if err != nil {
		return nil, errors.Wrap(err, "failed to start private validator")
	}

	return pvsc, nil
}

// SplitAndTrimEmpty slices s into all subslices separated by sep and returns a
// slice of the string s with all leading and trailing Unicode code points
// contained in cutset removed. If sep is empty, SplitAndTrim splits after each
// UTF-8 sequence. First part is equivalent to strings.SplitN with a count of
// -1.  also filter out empty strings, only return non-empty strings.
func SplitAndTrimEmpty(s, sep, cutset string) []string {
	if s == "" {
		return []string{}
	}

	spl := strings.Split(s, sep)
	nonEmptyStrings := make([]string, 0, len(spl))
	for i := 0; i < len(spl); i++ {
		element := strings.Trim(spl[i], cutset)
		if element != "" {
			nonEmptyStrings = append(nonEmptyStrings, element)
		}
	}
	return nonEmptyStrings
}
