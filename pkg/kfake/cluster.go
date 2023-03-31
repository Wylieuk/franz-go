package kfake

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kmsg"
)

// TODO
//
// * Handle requests concurrently, i.e. JoinGroup
//   * Actually, just spin out concurrent group manager that then hooks back
//     into the control loop
//
// * Add raft and make the brokers independent
//
// * Support multiple replicas -- we just pass this through
// * Support per-partition leader epoch

type (

	// Cluster is a mock Kafka broker cluster.
	Cluster struct {
		cfg cfg

		controller *broker
		bs         []*broker

		adminCh      chan func()
		reqCh        chan clientReq
		watchFetchCh chan *watchFetch

		controlMu          sync.Mutex
		control            map[int16][]controlFn
		keepCurrentControl atomic.Bool

		data data
		pids pids

		die  chan struct{}
		dead atomic.Bool
	}

	broker struct {
		c     *Cluster
		ln    net.Listener
		node  int32
		bsIdx int
	}

	controlFn func(kmsg.Request) (kmsg.Response, error, bool)
)

// MustCluster is like NewCluster, but panics on error.
func MustCluster(opts ...Opt) *Cluster {
	c, err := NewCluster(opts...)
	if err != nil {
		panic(err)
	}
	return c
}

// NewCluster returns a new mocked Kafka cluster.
func NewCluster(opts ...Opt) (c *Cluster, err error) {
	cfg := cfg{
		nbrokers:        3,
		logger:          new(nopLogger),
		clusterID:       "kfake",
		defaultNumParts: 10,
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if len(cfg.ports) > 0 {
		cfg.nbrokers = len(cfg.ports)
	}

	c = &Cluster{
		cfg: cfg,

		adminCh:      make(chan func()),
		reqCh:        make(chan clientReq, 20),
		watchFetchCh: make(chan *watchFetch, 20),
		control:      make(map[int16][]controlFn),

		data: data{
			id2t:      make(map[uuid]string),
			t2id:      make(map[string]uuid),
			treplicas: make(map[string]int),
		},

		die: make(chan struct{}),
	}
	c.data.c = c
	defer func() {
		if err != nil {
			c.Close()
		}
	}()

	for i := 0; i < cfg.nbrokers; i++ {
		var port int
		if len(cfg.ports) > 0 {
			port = cfg.ports[i]
		}
		ln, err := newListener(port)
		if err != nil {
			c.Close()
			return nil, err
		}
		b := &broker{
			c:     c,
			ln:    ln,
			node:  int32(i),
			bsIdx: len(c.bs),
		}
		c.bs = append(c.bs, b)
		go b.listen()
	}
	c.controller = c.bs[len(c.bs)-1]
	go c.run()
	return c, nil
}

// ListenAddrs returns the hostports that the cluster is listening on.
func (c *Cluster) ListenAddrs() []string {
	var addrs []string
	c.admin(func() {
		for _, b := range c.bs {
			addrs = append(addrs, b.ln.Addr().String())
		}
	})
	return addrs
}

// Close shuts down the cluster.
func (c *Cluster) Close() {
	if c.dead.Swap(true) {
		return
	}
	close(c.die)
	for _, b := range c.bs {
		b.ln.Close()
	}
}

func newListener(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func (b *broker) listen() {
	defer b.ln.Close()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}

		cc := &clientConn{
			c:      b.c,
			b:      b,
			conn:   conn,
			respCh: make(chan clientResp, 2),
		}
		go cc.read()
		go cc.write()
	}
}

type clientReq struct {
	cc   *clientConn
	kreq kmsg.Request
	at   time.Time
	corr int32
}
type clientResp struct {
	kresp kmsg.Response
	corr  int32
	err   error
}

func (c *Cluster) run() {
	for {
		var creq clientReq
		var w *watchFetch

		select {
		case creq = <-c.reqCh:
		case w = <-c.watchFetchCh:
			if w.cleaned {
				continue // already cleaned up, this is an extraneous timer fire
			}
			w.cleanup(c)
			creq = w.creq
		case <-c.die:
			return
		case fn := <-c.adminCh:
			// Run a custom request in the context of the cluster
			fn()
			continue
		}

		kreq := creq.kreq
		kresp, err, handled := c.tryControl(kreq)
		if handled {
			goto afterControl
		}

		switch k := kmsg.Key(kreq.Key()); k {
		case kmsg.Produce:
			kresp, err = c.handleProduce(creq.cc.b, kreq)
		case kmsg.Fetch:
			kresp, err = c.handleFetch(creq, w)
		case kmsg.ListOffsets:
			kresp, err = c.handleListOffsets(creq.cc.b, kreq)
		case kmsg.Metadata:
			kresp, err = c.handleMetadata(kreq)
		case kmsg.ApiVersions:
			kresp, err = c.handleApiVersions(kreq)
		case kmsg.CreateTopics:
			kresp, err = c.handleCreateTopics(creq.cc.b, kreq)
		case kmsg.DeleteTopics:
			kresp, err = c.handleDeleteTopics(creq.cc.b, kreq)
		case kmsg.InitProducerID:
			kresp, err = c.handleInitProducerID(kreq)
		case kmsg.OffsetForLeaderEpoch:
			kresp, err = c.handleOffsetForLeaderEpoch(creq.cc.b, kreq)
		case kmsg.CreatePartitions:
			kresp, err = c.handleCreatePartitions(creq.cc.b, kreq)
		default:
			err = fmt.Errorf("unahndled key %v", k)
		}

	afterControl:
		if kresp == nil && err == nil { // produce request with no acks
			continue
		}

		select {
		case creq.cc.respCh <- clientResp{kresp: kresp, corr: creq.corr, err: err}:
		case <-c.die:
			return
		}
	}
}

// Control is a function to call on any client request the cluster handles.
//
// If the control function returns true, then either the response is written
// back to the client or, if there the control function returns an error, the
// client connection is closed. If both returns are nil, then the cluster will
// loop continuing to read from the client and the client will likely have a
// read timeout at some point.
//
// Controlling a request drops the control function from the cluster, meaning
// that a control function can only control *one* request. To keep the control
// function handling more requests, you can call KeepControl within your
// control function.
//
// It is safe to add new control functions within a control function. Control
// functions are not called concurrently.
func (c *Cluster) Control(fn func(kmsg.Request) (kmsg.Response, error, bool)) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	c.control[-1] = append(c.control[-1], fn)
}

// Control is a function to call on a specific request key that the cluster
// handles.
//
// If the control function returns true, then either the response is written
// back to the client or, if there the control function returns an error, the
// client connection is closed. If both returns are nil, then the cluster will
// loop continuing to read from the client and the client will likely have a
// read timeout at some point.
//
// Controlling a request drops the control function from the cluster, meaning
// that a control function can only control *one* request. To keep the control
// function handling more requests, you can call KeepControl within your
// control function.
//
// It is safe to add new control functions within a control function.
func (c *Cluster) ControlKey(key int16, fn func(kmsg.Request) (kmsg.Response, error, bool)) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	c.control[key] = append(c.control[key], fn)
}

// KeepControl marks the currently running control function to be kept even if
// you handle the request and return true. This can be used to continuously
// control requests without needing to re-add control functions manually.
func (c *Cluster) KeepControl() {
	c.keepCurrentControl.Swap(true)
}

func (c *Cluster) tryControl(kreq kmsg.Request) (kresp kmsg.Response, err error, handled bool) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if len(c.control) == 0 {
		return nil, nil, false
	}

	keyFns := c.control[kreq.Key()]
	for i, fn := range keyFns {
		kresp, err, handled = c.callControl(kreq.Key(), kreq, fn)
		if handled {
			c.control[kreq.Key()] = append(keyFns[:i], keyFns[i+1:]...)
			return
		}
	}
	anyFns := c.control[-1]
	for i, fn := range anyFns {
		kresp, err, handled = c.callControl(-1, kreq, fn)
		if handled {
			c.control[-1] = append(anyFns[:i], anyFns[i+1:]...)
			return
		}
	}
	return
}

func (c *Cluster) callControl(key int16, req kmsg.Request, fn controlFn) (kresp kmsg.Response, err error, handled bool) {
	c.keepCurrentControl.Swap(false)
	c.controlMu.Unlock()
	defer func() {
		c.controlMu.Lock()
		if handled && c.keepCurrentControl.Swap(false) {
			c.control[key] = append(c.control[key], fn)
		}
	}()
	return fn(req)
}

// Various administrative requests can be passed into the cluster to simulate
// real-world operations. These are performed synchronously in the goroutine
// that handles client requests.

func (c *Cluster) admin(fn func()) {
	ofn := fn
	wait := make(chan struct{})
	fn = func() { ofn(); close(wait) }
	c.adminCh <- fn
	<-wait
}

// MoveTopicPartition simulates the rebalancing of a partition to an alternative
// broker. This returns an error if the topic, partition, or node does not exit.
func (c *Cluster) MoveTopicPartition(topic string, partition int32, nodeID int32) error {
	var err error
	c.admin(func() {
		var br *broker
		for _, b := range c.bs {
			if b.node == nodeID {
				br = b
				break
			}
		}
		if br == nil {
			err = fmt.Errorf("node %d not found", nodeID)
			return
		}
		pd, ok := c.data.tps.getp(topic, partition)
		if !ok {
			err = errors.New("topic/partition not found")
			return
		}
		pd.leader = br
	})
	return err
}

// AddNode adds a node to the cluster. If nodeID is -1, the next node ID is
// used. If port is 0 or negative, a random port is chosen. This returns the
// added node ID and the port used, or an error if the node already exists or
// the port cannot be listened to.
func (c *Cluster) AddNode(nodeID int32, port int) (int32, int, error) {
	var err error
	c.admin(func() {
		if nodeID >= 0 {
			for _, b := range c.bs {
				if b.node == nodeID {
					err = fmt.Errorf("node %d already exists", nodeID)
					return
				}
			}
		} else if len(c.bs) > 0 {
			// We go one higher than the max current node ID. We
			// need to search all nodes because a person may have
			// added and removed a bunch, with manual ID overrides.
			nodeID = c.bs[0].node
			for _, b := range c.bs[1:] {
				if b.node > nodeID {
					nodeID = b.node
				}
			}
			nodeID++
		} else {
			nodeID = 0
		}
		if port < 0 {
			port = 0
		}
		var ln net.Listener
		if ln, err = newListener(port); err != nil {
			return
		}
		_, strPort, _ := net.SplitHostPort(ln.Addr().String())
		port, _ = strconv.Atoi(strPort)
		b := &broker{
			c:     c,
			ln:    ln,
			node:  nodeID,
			bsIdx: len(c.bs),
		}
		c.bs = append(c.bs, b)
		c.cfg.nbrokers++
		c.shufflePartitionsLocked()
		go b.listen()
	})
	return nodeID, port, err
}

// RemoveNode removes a ndoe from the cluster. This returns an error if the
// node does not exist.
func (c *Cluster) RemoveNode(nodeID int32) error {
	var err error
	c.admin(func() {
		for i, b := range c.bs {
			if b.node == nodeID {
				if len(c.bs) == 1 {
					err = errors.New("cannot remove all brokers")
					return
				}
				b.ln.Close()
				c.cfg.nbrokers--
				c.bs[i] = c.bs[len(c.bs)-1]
				c.bs[i].bsIdx = i
				c.bs = c.bs[:len(c.bs)-1]
				c.shufflePartitionsLocked()
				return
			}
		}
		err = fmt.Errorf("node %d not found", nodeID)
	})
	return err
}

// ShufflePartitionLeaders simulates a leader election for all partitions: all
// partitions have a randomly selected new leader and their internal epochs are
// bumped.
func (c *Cluster) ShufflePartitionLeaders() {
	c.admin(func() {
		c.shufflePartitionsLocked()
	})
}

func (c *Cluster) shufflePartitionsLocked() {
	c.data.tps.each(func(_ string, _ int32, p *partData) {
		var leader *broker
		if len(c.bs) == 0 {
			leader = c.noLeader()
		} else {
			leader = c.bs[rand.Intn(len(c.bs))]
		}
		p.leader = leader
		p.epoch++
	})
}