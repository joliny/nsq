package main

import (
	"bufio"
	"compress/flate"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bitly/go-nsq"
	"github.com/mreiferson/go-snappystream"
)

const DefaultBufferSize = 16 * 1024

type IdentifyDataV2 struct {
	ShortId             string `json:"short_id"`
	LongId              string `json:"long_id"`
	HeartbeatInterval   int    `json:"heartbeat_interval"`
	OutputBufferSize    int    `json:"output_buffer_size"`
	OutputBufferTimeout int    `json:"output_buffer_timeout"`
	FeatureNegotiation  bool   `json:"feature_negotiation"`
	TLSv1               bool   `json:"tls_v1"`
	Deflate             bool   `json:"deflate"`
	DeflateLevel        int    `json:"deflate_level"`
	Snappy              bool   `json:"snappy"`
	SampleRate          int32  `json:"sample_rate"`
	UserAgent           string `json:"user_agent"`
	MsgTimeout          int    `json:"msg_timeout"`
}

type IdentifyEvent struct {
	OutputBufferTimeout time.Duration
	HeartbeatInterval   time.Duration
	SampleRate          int32
	MsgTimeout          time.Duration
}

type ClientV2 struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	ReadyCount     int64
	LastReadyCount int64
	InFlightCount  int64
	MessageCount   uint64
	FinishCount    uint64
	RequeueCount   uint64

	sync.RWMutex

	ID        int64
	context   *Context
	UserAgent string

	// original connection
	net.Conn

	// connections based on negotiated features
	tlsConn     *tls.Conn
	flateWriter *flate.Writer

	// reading/writing interfaces
	Reader *bufio.Reader
	Writer *bufio.Writer

	OutputBufferSize    int
	OutputBufferTimeout time.Duration

	HeartbeatInterval time.Duration

	MsgTimeout time.Duration

	State           int32
	ConnectTime     time.Time
	Channel         *Channel
	ReadyStateChan  chan int
	ExitChan        chan int
	ShortIdentifier string
	LongIdentifier  string
	SampleRate      int32

	IdentifyEventChan chan IdentifyEvent
	SubEventChan      chan *Channel

	TLS     int32
	Snappy  int32
	Deflate int32

	// re-usable buffer for reading the 4-byte lengths off the wire
	lenBuf   [4]byte
	lenSlice []byte
}

func NewClientV2(id int64, conn net.Conn, context *Context) *ClientV2 {
	var identifier string
	if conn != nil {
		identifier, _, _ = net.SplitHostPort(conn.RemoteAddr().String())
	}

	c := &ClientV2{
		ID:      id,
		context: context,

		Conn: conn,

		Reader: bufio.NewReaderSize(conn, DefaultBufferSize),
		Writer: bufio.NewWriterSize(conn, DefaultBufferSize),

		OutputBufferSize:    DefaultBufferSize,
		OutputBufferTimeout: 250 * time.Millisecond,

		MsgTimeout: context.nsqd.options.MsgTimeout,

		// ReadyStateChan has a buffer of 1 to guarantee that in the event
		// there is a race the state update is not lost
		ReadyStateChan:  make(chan int, 1),
		ExitChan:        make(chan int),
		ConnectTime:     time.Now(),
		ShortIdentifier: identifier,
		LongIdentifier:  identifier,
		State:           nsq.StateInit,

		SubEventChan:      make(chan *Channel, 1),
		IdentifyEventChan: make(chan IdentifyEvent, 1),

		// heartbeats are client configurable but default to 30s
		HeartbeatInterval: context.nsqd.options.ClientTimeout / 2,
	}
	c.lenSlice = c.lenBuf[:]
	return c
}

func (c *ClientV2) String() string {
	return c.RemoteAddr().String()
}

func (c *ClientV2) Identify(data IdentifyDataV2) error {
	c.Lock()
	c.ShortIdentifier = data.ShortId
	c.LongIdentifier = data.LongId
	c.UserAgent = data.UserAgent
	c.Unlock()

	err := c.SetHeartbeatInterval(data.HeartbeatInterval)
	if err != nil {
		return err
	}

	err = c.SetOutputBufferSize(data.OutputBufferSize)
	if err != nil {
		return err
	}

	err = c.SetOutputBufferTimeout(data.OutputBufferTimeout)
	if err != nil {
		return err
	}

	err = c.SetSampleRate(data.SampleRate)
	if err != nil {
		return err
	}

	err = c.SetMsgTimeout(data.MsgTimeout)
	if err != nil {
		return err
	}

	ie := IdentifyEvent{
		OutputBufferTimeout: c.OutputBufferTimeout,
		HeartbeatInterval:   c.HeartbeatInterval,
		SampleRate:          c.SampleRate,
		MsgTimeout:          c.MsgTimeout,
	}

	// update the client's message pump
	select {
	case c.IdentifyEventChan <- ie:
	default:
	}

	return nil
}

func (c *ClientV2) Stats() ClientStats {
	c.RLock()
	name := c.ShortIdentifier
	userAgent := c.UserAgent
	c.RUnlock()
	return ClientStats{
		Version:       "V2",
		RemoteAddress: c.RemoteAddr().String(),
		Name:          name,
		UserAgent:     userAgent,
		State:         atomic.LoadInt32(&c.State),
		ReadyCount:    atomic.LoadInt64(&c.ReadyCount),
		InFlightCount: atomic.LoadInt64(&c.InFlightCount),
		MessageCount:  atomic.LoadUint64(&c.MessageCount),
		FinishCount:   atomic.LoadUint64(&c.FinishCount),
		RequeueCount:  atomic.LoadUint64(&c.RequeueCount),
		ConnectTime:   c.ConnectTime.Unix(),
		SampleRate:    atomic.LoadInt32(&c.SampleRate),
		TLS:           atomic.LoadInt32(&c.TLS) == 1,
		Deflate:       atomic.LoadInt32(&c.Deflate) == 1,
		Snappy:        atomic.LoadInt32(&c.Snappy) == 1,
	}
}

func (c *ClientV2) IsReadyForMessages() bool {
	if c.Channel.IsPaused() {
		return false
	}

	readyCount := atomic.LoadInt64(&c.ReadyCount)
	lastReadyCount := atomic.LoadInt64(&c.LastReadyCount)
	inFlightCount := atomic.LoadInt64(&c.InFlightCount)

	if *verbose {
		log.Printf("[%s] state rdy: %4d lastrdy: %4d inflt: %4d", c,
			readyCount, lastReadyCount, inFlightCount)
	}

	if inFlightCount >= lastReadyCount || readyCount <= 0 {
		return false
	}

	return true
}

func (c *ClientV2) SetReadyCount(count int64) {
	atomic.StoreInt64(&c.ReadyCount, count)
	atomic.StoreInt64(&c.LastReadyCount, count)
	c.tryUpdateReadyState()
}

func (c *ClientV2) tryUpdateReadyState() {
	// you can always *try* to write to ReadyStateChan because in the cases
	// where you cannot the message pump loop would have iterated anyway.
	// the atomic integer operations guarantee correctness of the value.
	select {
	case c.ReadyStateChan <- 1:
	default:
	}
}

func (c *ClientV2) FinishedMessage() {
	atomic.AddUint64(&c.FinishCount, 1)
	atomic.AddInt64(&c.InFlightCount, -1)
	c.tryUpdateReadyState()
}

func (c *ClientV2) Empty() {
	atomic.StoreInt64(&c.InFlightCount, 0)
	c.tryUpdateReadyState()
}

func (c *ClientV2) SendingMessage() {
	atomic.AddInt64(&c.ReadyCount, -1)
	atomic.AddInt64(&c.InFlightCount, 1)
	atomic.AddUint64(&c.MessageCount, 1)
}

func (c *ClientV2) TimedOutMessage() {
	atomic.AddInt64(&c.InFlightCount, -1)
	c.tryUpdateReadyState()
}

func (c *ClientV2) RequeuedMessage() {
	atomic.AddUint64(&c.RequeueCount, 1)
	atomic.AddInt64(&c.InFlightCount, -1)
	c.tryUpdateReadyState()
}

func (c *ClientV2) StartClose() {
	// Force the client into ready 0
	c.SetReadyCount(0)
	// mark this client as closing
	atomic.StoreInt32(&c.State, nsq.StateClosing)
}

func (c *ClientV2) Pause() {
	c.tryUpdateReadyState()
}

func (c *ClientV2) UnPause() {
	c.tryUpdateReadyState()
}

func (c *ClientV2) SetHeartbeatInterval(desiredInterval int) error {
	c.Lock()
	defer c.Unlock()

	switch {
	case desiredInterval == -1:
		c.HeartbeatInterval = 0
	case desiredInterval == 0:
		// do nothing (use default)
	case desiredInterval >= 1000 &&
		desiredInterval <= int(c.context.nsqd.options.MaxHeartbeatInterval/time.Millisecond):
		c.HeartbeatInterval = time.Duration(desiredInterval) * time.Millisecond
	default:
		return errors.New(fmt.Sprintf("heartbeat interval (%d) is invalid", desiredInterval))
	}

	return nil
}

func (c *ClientV2) SetOutputBufferSize(desiredSize int) error {
	var size int

	switch {
	case desiredSize == -1:
		// effectively no buffer (every write will go directly to the wrapped net.Conn)
		size = 1
	case desiredSize == 0:
		// do nothing (use default)
	case desiredSize >= 64 && desiredSize <= int(c.context.nsqd.options.MaxOutputBufferSize):
		size = desiredSize
	default:
		return errors.New(fmt.Sprintf("output buffer size (%d) is invalid", desiredSize))
	}

	if size > 0 {
		c.Lock()
		defer c.Unlock()
		c.OutputBufferSize = size
		err := c.Writer.Flush()
		if err != nil {
			return err
		}
		c.Writer = bufio.NewWriterSize(c.Conn, size)
	}

	return nil
}

func (c *ClientV2) SetOutputBufferTimeout(desiredTimeout int) error {
	c.Lock()
	defer c.Unlock()

	switch {
	case desiredTimeout == -1:
		c.OutputBufferTimeout = 0
	case desiredTimeout == 0:
		// do nothing (use default)
	case desiredTimeout >= 1 &&
		desiredTimeout <= int(c.context.nsqd.options.MaxOutputBufferTimeout/time.Millisecond):
		c.OutputBufferTimeout = time.Duration(desiredTimeout) * time.Millisecond
	default:
		return errors.New(fmt.Sprintf("output buffer timeout (%d) is invalid", desiredTimeout))
	}

	return nil
}

func (c *ClientV2) SetSampleRate(sampleRate int32) error {
	if sampleRate < 0 || sampleRate > 99 {
		return errors.New(fmt.Sprintf("sample rate (%d) is invalid", sampleRate))
	}
	atomic.StoreInt32(&c.SampleRate, sampleRate)
	return nil
}

func (c *ClientV2) SetMsgTimeout(msgTimeout int) error {
	c.Lock()
	defer c.Unlock()

	switch {
	case msgTimeout == 0:
		// do nothing (use default)
	case msgTimeout >= 1000 &&
		msgTimeout <= int(c.context.nsqd.options.MaxMsgTimeout/time.Millisecond):
		c.MsgTimeout = time.Duration(msgTimeout) * time.Millisecond
	default:
		return errors.New(fmt.Sprintf("msg timeout (%d) is invalid", msgTimeout))
	}

	return nil
}

func (c *ClientV2) UpgradeTLS() error {
	c.Lock()
	defer c.Unlock()

	tlsConn := tls.Server(c.Conn, c.context.nsqd.tlsConfig)
	err := tlsConn.Handshake()
	if err != nil {
		return err
	}
	c.tlsConn = tlsConn

	c.Reader = bufio.NewReaderSize(c.tlsConn, DefaultBufferSize)
	c.Writer = bufio.NewWriterSize(c.tlsConn, c.OutputBufferSize)

	atomic.StoreInt32(&c.TLS, 1)

	return nil
}

func (c *ClientV2) UpgradeDeflate(level int) error {
	c.Lock()
	defer c.Unlock()

	conn := c.Conn
	if c.tlsConn != nil {
		conn = c.tlsConn
	}

	c.Reader = bufio.NewReaderSize(flate.NewReader(conn), DefaultBufferSize)

	fw, _ := flate.NewWriter(conn, level)
	c.flateWriter = fw
	c.Writer = bufio.NewWriterSize(fw, c.OutputBufferSize)

	atomic.StoreInt32(&c.Deflate, 1)

	return nil
}

func (c *ClientV2) UpgradeSnappy() error {
	c.Lock()
	defer c.Unlock()

	conn := c.Conn
	if c.tlsConn != nil {
		conn = c.tlsConn
	}

	c.Reader = bufio.NewReaderSize(snappystream.NewReader(conn, snappystream.SkipVerifyChecksum), DefaultBufferSize)
	c.Writer = bufio.NewWriterSize(snappystream.NewWriter(conn), c.OutputBufferSize)

	atomic.StoreInt32(&c.Snappy, 1)

	return nil
}

func (c *ClientV2) Flush() error {
	c.SetWriteDeadline(time.Now().Add(time.Second))

	err := c.Writer.Flush()
	if err != nil {
		return err
	}

	if c.flateWriter != nil {
		return c.flateWriter.Flush()
	}

	return nil
}
