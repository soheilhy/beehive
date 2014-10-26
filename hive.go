package beehive

import (
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/soheilhy/beehive/Godeps/_workspace/src/code.google.com/p/go.net/context"
	etcdraft "github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/coreos/etcd/raft"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/coreos/etcd/raft/raftpb"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/golang/glog"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/gorilla/mux"
	bhflag "github.com/soheilhy/beehive/flag"
	bhgob "github.com/soheilhy/beehive/gob"
	"github.com/soheilhy/beehive/raft"
)

const (
	defaultRaftTick = 30 * time.Millisecond
)

type Hive interface {
	// ID of the hive. Valid only if the hive is started.
	ID() uint64
	// Config returns the hive configuration.
	Config() HiveConfig

	// Start starts the hive. This function blocks.
	Start() error
	// Stop stops the hive and all its apps. It blocks until the hive is actually
	// stopped.
	Stop() error

	// Creates an app with the given name. Note that apps are not active until
	// the hive is started.
	NewApp(name string) App

	// Emits a message containing msgData from this hive.
	Emit(msgData interface{})
	// Sends a message to a specific bee that owns a specific dictionary key.
	SendToCellKey(msgData interface{}, to string, dk CellKey)
	// Sends a message to a sepcific bee.
	SendToBee(msgData interface{}, to uint64)
	// Replies to a message.
	ReplyTo(msg Msg, replyData interface{}) error

	// Registers a message for encoding/decoding. This method should be called
	// only on messages that have no active handler. Such messages are almost
	// always replies to some detached handler.
	RegisterMsg(msg interface{})
}

// Configuration of a hive.
type HiveConfig struct {
	Addr            string        // Listening address of the hive.
	PeerAddrs       []string      // Peer addresses.
	RegAddrs        []string      // Reigstery service addresses.
	StatePath       string        // Where to store state data.
	DataChBufSize   int           // Buffer size of the data channels.
	CmdChBufSize    int           // Buffer size of the control channels.
	Instrument      bool          // Whether to instrument apps on the hive.
	HBQueryInterval time.Duration // Heartbeating interval.
	HBDeadTimeout   time.Duration // When to announce a bee dead.
	RegLockTimeout  time.Duration // When to retry to lock an entry in a registry.
	UseBeeHeartbeat bool          // Heartbeat bees instead of the registry.
}

// Creates a new hive based on the given configuration.
func NewHiveWithConfig(cfg HiveConfig) Hive {
	os.MkdirAll(cfg.StatePath, 0700)
	m := meta(cfg)
	h := &hive{
		id:     m.Hive.ID,
		meta:   m,
		status: hiveStopped,
		config: cfg,
		dataCh: make(chan *msg, cfg.DataChBufSize),
		ctrlCh: make(chan cmdAndChannel),
		apps:   make(map[string]*app, 0),
		qees:   make(map[string][]qeeAndHandler),
		ticker: time.NewTicker(defaultRaftTick),
		client: newHttpClient(),
	}

	h.registry = newRegistry()
	h.replStrategy = newRndReplication(h)

	gob.Register(Colony{})
	gob.Register(msg{})
	gob.Register(cmd{})
	gob.Register(cmdResult{})
	gob.Register(cmdStop{})
	gob.Register(cmdStart{})
	gob.Register(cmdSync{})
	gob.Register(cmdFindBee{})
	gob.Register(cmdNewHiveID{})
	gob.Register(cmdAddHive{})
	gob.Register(cmdCreateBee{})
	gob.Register(cmdReloadBee{})
	gob.Register(cmdLiveHives{})
	gob.Register(cmdJoinColony{})
	gob.Register(bhgob.GobError{})
	gob.Register(commitTx{})

	// FIXME REFACTOR
	//gob.Register(joinColonyCmd{})
	//gob.Register(bufferTxCmd{})
	//gob.Register(commitTxCmd{})
	//gob.Register(getTxInfoCmd{})
	//gob.Register(getTx{})
	//gob.Register(migrateBeeCmd{})
	//gob.Register(replaceBeeCmd{})
	//gob.Register(lockMappedCellsCmd{})
	//gob.Register(getColonyCmd{})
	//gob.Register(addSlaveCmd{})
	//gob.Register(delSlaveCmd{})
	//gob.Register(addMappedCells{})

	//if h.config.Instrument {
	//h.collector = newAppStatCollector(h)
	//} else {
	//h.collector = &dummyStatCollector{}
	//}
	//startHeartbeatHandler(h)
	//h.replStrategy = newRndReplication(h)

	return h
}

var DefaultCfg = HiveConfig{}

// Create a new hive and load its configuration from command line flags.
func NewHive() Hive {
	if !flag.Parsed() {
		flag.Parse()
	}

	return NewHiveWithConfig(DefaultCfg)
}

func init() {
	flag.StringVar(&DefaultCfg.Addr, "laddr", "localhost:7767",
		"The listening address used to communicate with other nodes")
	flag.Var(&bhflag.CSV{&DefaultCfg.PeerAddrs}, "paddrs",
		"Address of peers. Seperate entries with a comma")
	flag.Var(&bhflag.CSV{&DefaultCfg.RegAddrs}, "raddrs",
		"Address of etcd machines. Separate entries with a comma ','")
	flag.IntVar(&DefaultCfg.DataChBufSize, "chsize", 1024,
		"Buffer size of data channels")
	flag.IntVar(&DefaultCfg.CmdChBufSize, "cmdchsize", 128,
		"Buffer size of command channels")
	flag.BoolVar(&DefaultCfg.Instrument, "instrument", false,
		"Whether to insturment apps")
	flag.StringVar(&DefaultCfg.StatePath, "statepath", "/tmp/beehive",
		"Where to store persistent state data")
	flag.DurationVar(&DefaultCfg.HBQueryInterval, "hbqueryinterval",
		100*time.Millisecond, "Heartbeat interval")
	flag.DurationVar(&DefaultCfg.HBDeadTimeout, "hbdeadtimeout",
		300*time.Millisecond,
		"The timeout after which a non-responsive bee is announced dead")
	flag.DurationVar(&DefaultCfg.RegLockTimeout, "reglocktimeout",
		10*time.Millisecond, "Timeout to retry locking an entry in the registry")
	flag.BoolVar(&DefaultCfg.UseBeeHeartbeat, "userbeehb", false,
		"Whether to use high-granular bee heartbeating in addition to registry"+
			"events")
}

type qeeAndHandler struct {
	q *qee
	h Handler
}

// hiveStatus represents the status of a hive.
type hiveStatus int

// Valid values for HiveStatus.
const (
	hiveStopped hiveStatus = iota
	hiveStarted            = iota
)

// The internal implementation of Hive.
type hive struct {
	id     uint64
	meta   hiveMeta
	config HiveConfig

	status hiveStatus

	dataCh chan *msg
	ctrlCh chan cmdAndChannel
	sigCh  chan os.Signal

	apps map[string]*app
	qees map[string][]qeeAndHandler

	node     *raft.Node
	registry *registry
	ticker   *time.Ticker
	listener net.Listener
	client   *http.Client

	replStrategy replicationStrategy
	// FIXME REFACTOR
	//collector statCollector
}

func (h *hive) ID() uint64 {
	return h.id
}

func (h *hive) String() string {
	return fmt.Sprintf("hive %v@%v", h.id, h.config.Addr)
}

func (h *hive) Config() HiveConfig {
	return h.config
}

func (h *hive) RegisterMsg(msg interface{}) {
	gob.Register(msg)
}

// FIXME REFACTOR
//func (h *hive) isolated() bool {
//return !h.registry.connected()
//}

func (h *hive) app(name string) (*app, bool) {
	a, ok := h.apps[name]
	return a, ok
}

func (h *hive) hiveAddr(id uint64) (string, error) {
	i, err := h.registry.hive(id)
	return i.Addr, err
}

func (h *hive) stopListener() {
	glog.Infof("%v closes listener...", h)
	if h.listener != nil {
		h.listener.Close()
	}
}

func (h *hive) stopQees() {
	glog.Infof("%v is stopping qees...", h)
	qs := make(map[*qee]bool)
	for _, mhs := range h.qees {
		for _, mh := range mhs {
			qs[mh.q] = true
		}
	}

	stopCh := make(chan cmdResult)
	for q, _ := range qs {
		q.ctrlCh <- newCmdAndChannel(cmdStop{}, q.app.Name(), 0, stopCh)
		glog.V(3).Infof("Waiting on a qee: %v", q)
		stopped := false
		tries := 5
		for !stopped {
			select {
			case res := <-stopCh:
				_, err := res.get()
				if err != nil {
					glog.Errorf("Error in stopping a qee: %v", err)
				}
				stopped = true
			case <-time.After(1 * time.Second):
				if tries--; tries < 0 {
					glog.Infof("Giving up on qee %v", q)
					stopped = true
					continue
				}
				glog.Infof("Still waiting for a qee %v...", q)
			}
		}
	}
}

func (h *hive) handleCmd(cc cmdAndChannel) {
	glog.V(2).Infof("%v handles cmd %+v", h, cc.cmd)
	switch d := cc.cmd.Data.(type) {
	case cmdStop:
		// TODO(soheil): This has a race with Stop(). Use atomics here.
		h.status = hiveStopped
		h.stopListener()
		h.stopQees()
		h.node.Stop()
		cc.ch <- cmdResult{}

	case cmdPingHive:
		cc.ch <- cmdResult{}

	case cmdSync:
		err := h.raftBarrier()
		cc.ch <- cmdResult{Err: err}

	case cmdNewHiveID:
		r, err := h.node.Process(context.TODO(), newHiveID{d.Addr})
		cc.ch <- cmdResult{
			Data: r,
			Err:  err,
		}

	case cmdAddHive:
		err := h.node.AddNode(context.TODO(), d.Info.ID, d.Info.Addr)
		cc.ch <- cmdResult{
			Err: err,
		}

	case cmdLiveHives:
		cc.ch <- cmdResult{
			Data: h.registry.hives(),
		}

	default:
		cc.ch <- cmdResult{
			Err: ErrInvalidCmd,
		}
	}
}

func (h *hive) processCmd(data interface{}) (interface{}, error) {
	ch := make(chan cmdResult)
	h.ctrlCh <- newCmdAndChannel(data, "", 0, ch)
	return (<-ch).get()
}

func (h *hive) processRaft(ctx context.Context, msg raftpb.Message) error {
	return h.node.Step(ctx, msg)
}

func (h *hive) raftBarrier() error {
	ctx, _ := context.WithTimeout(context.Background(), defaultRaftTick*300)
	_, err := h.node.Process(ctx, noOp{})
	return err
}

func (h *hive) registerApp(a *app) {
	h.apps[a.Name()] = a
}

func (h *hive) registerHandler(t string, q *qee, l Handler) {
	for i, qh := range h.qees[t] {
		if qh.q == q {
			h.qees[t][i].h = l
			return
		}
	}

	h.qees[t] = append(h.qees[t], qeeAndHandler{q, l})
}

func (h *hive) bee(id uint64) (BeeInfo, error) {
	return h.registry.bee(id)
}

func (h *hive) handleMsg(m *msg) {
	switch {
	case m.IsUnicast():
		i, err := h.bee(m.MsgTo)
		if err != nil {
			glog.Errorf("no such bee %v", m.MsgTo)
		}
		a, ok := h.app(i.App)
		if !ok {
			glog.Fatalf("no such application %s", i.App)
		}
		a.qee.dataCh <- msgAndHandler{m, a.handler(m.Type())}
	default:
		for _, qh := range h.qees[m.Type()] {
			qh.q.dataCh <- msgAndHandler{m, qh.h}
		}
	}
}

func (h *hive) startQees() {
	for _, a := range h.apps {
		go a.qee.start()
	}
}

func (h *hive) startRaftNode() {
	peers := make([]etcdraft.Peer, 0, 1)
	if len(h.meta.Peers) != 0 {
		h.registry.initHives(h.meta.Peers)
	} else {
		peers = append(peers, raft.NodeInfo(h.info()).Peer())
	}
	h.node = raft.NewNode(h.id, peers, h.sendRaft, h.config.StatePath,
		h.registry, 1024, h.ticker.C)
	// This will act like a barrier.
	if err := h.raftBarrier(); err != nil {
		glog.Fatalf("error when joining the cluster: %v", err)
	}
	glog.V(2).Infof("%v is in sync with the cluster", h)
}

func (h *hive) reloadState() {
	for _, b := range h.registry.beesOfHive(h.id) {
		if b.Detached {
			glog.Warningf("%v will not reload detached bee %v", h, b.ID)
			continue
		}
		a, ok := h.app(b.App)
		if !ok {
			glog.Errorf("app %v is not registered but has a bee", b.App)
			continue
		}
		_, err := a.qee.processCmd(cmdReloadBee{ID: b.ID})
		if err != nil {
			glog.Errorf("cannot reload bee %v on %v", b.ID, h.id)
			continue
		}
	}
}

func (h *hive) Start() error {
	h.status = hiveStarted
	h.registerSignals()
	if err := h.listen(); err != nil {
		glog.Errorf("%v cannot start listener: %v", err)
		h.Stop()
		return err
	}
	h.startRaftNode()
	h.startQees()
	h.reloadState()

	glog.V(2).Infof("%v starts message loop", h)
	for h.status == hiveStarted {
		select {
		case msg := <-h.dataCh:
			h.handleMsg(msg)

		case cmd := <-h.ctrlCh:
			h.handleCmd(cmd)
		}
	}

	return nil
}

func (h *hive) info() HiveInfo {
	return HiveInfo{
		ID:   h.id,
		Addr: h.config.Addr,
	}
}

func (h *hive) Stop() error {
	if h.ctrlCh == nil {
		return errors.New("control channel is closed")
	}

	if h.status == hiveStopped {
		return errors.New("hive is already stopped")
	}

	_, err := h.sendCmd(cmdStop{})
	return err
}

func (h *hive) waitUntilStarted() {
	h.sendCmd(cmdPingHive{})
}

func (h *hive) sendCmd(cmd interface{}) (interface{}, error) {
	ch := make(chan cmdResult)
	h.ctrlCh <- newCmdAndChannel(cmd, "", 0, ch)
	return (<-ch).get()
}

func (h *hive) NewApp(name string) App {
	a := &app{
		name:     name,
		hive:     h,
		handlers: make(map[string]Handler),
	}
	a.initQee()
	h.registerApp(a)
	a.SetFlags(AppFlagTransactional)

	// TODO REFACTOR
	//a.Handle(heartbeatReq{}, &heartbeatReqHandler{})
	//mod := &colonyModerator{h.config.RegLockTimeout}
	//a.Handle(beeFailed{}, mod)
	//a.Handle(HiveJoined{}, mod)
	//a.Handle(HiveLeft{}, mod)

	return a
}

func (h *hive) Emit(msgData interface{}) {
	h.emitMsg(&msg{MsgData: msgData})
}

func (h *hive) emitMsg(msg *msg) {
	h.dataCh <- msg
}

func (h *hive) SendToCellKey(msgData interface{}, to string, k CellKey) {
	// TODO(soheil): Implement this hive.SendTo.
	glog.Fatalf("FIXME implement SendToCellKey")
}

func (h *hive) SendToBee(msgData interface{}, to uint64) {
	h.emitMsg(newMsgFromData(msgData, 0, to))
}

// Reply to thatMsg with the provided replyData.
func (h *hive) ReplyTo(thatMsg Msg, replyData interface{}) error {
	m := thatMsg.(*msg)
	if m.NoReply() {
		return errors.New("cannot reply to this message")
	}

	h.emitMsg(newMsgFromData(replyData, 0, m.From()))
	return nil
}

func (s *hive) registerSignals() {
	s.sigCh = make(chan os.Signal, 1)
	signal.Notify(s.sigCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		<-s.sigCh
		s.Stop()
	}()
}

func (h *hive) listen() error {
	l, e := net.Listen("tcp", h.config.Addr)
	if e != nil {
		glog.Errorf("%v cannot listen: %v", h, e)
		return e
	}
	glog.Infof("%v listens", h)
	h.listener = l

	s := h.newServer(h.config.Addr)
	go func() {
		s.Serve(l)
		glog.Infof("%v closed listener", h)
	}()
	return nil
}

// NewServer creates a server for the given addr. It installs all required
// handlers for Beehive.
func (h *hive) newServer(addr string) *server {
	r := mux.NewRouter()
	s := server{
		Server: http.Server{
			Addr:    addr,
			Handler: r,
		},
		router: r,
		hive:   h,
	}

	handlerV1 := v1Handler{
		srv: &s,
	}
	handlerV1.Install(r)

	return &s
}

func (h *hive) sendRaft(msgs []raftpb.Message) {
	for _, m := range msgs {
		go func(m raftpb.Message) {
			a, err := h.hiveAddr(m.To)
			if err != nil {
				glog.Errorf("%v has no address for node %v", h, m.To)
				return
			}
			if a == h.config.Addr {
				h.registry.m.RLock()
				glog.Fatalf("cannot send message to itself %#v", h.registry.Hives)
			}
			glog.V(2).Infof("%v sends raft message %v to %v", h, m, a)
			if err = newProxyWithAddr(h.client, a).sendRaft(m); err != nil {
				glog.Errorf("error in sending a raft message: %v", err)
				return
			}
			glog.V(2).Infof("raft message sucessfully sent to %v", m.To)
		}(m)
	}
}