// Package simclient is a FIX 4.4 initiator used by cmd/fixsim and the
// integration tests. It signs its Logon the way the gateway expects and records
// everything the gateway sends back.
package simclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
	filestore "github.com/quickfixgo/quickfix/store/file"

	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
)

// Config describes one initiator session.
type Config struct {
	Host, Port   string // gateway (or chaos proxy) address
	SenderCompID string
	TargetCompID string
	Username     string
	Secret       string
	Password     string
	DictPath     string // path to FIX44.xml
	HeartBtInt   int    // seconds; default 30
	Reset        bool   // send ResetSeqNumFlag=Y on every logon
	StoreDir     string // persistent client store; empty = in-memory
	Logger       *slog.Logger
}

// Report is the subset of an ExecutionReport or OrderCancelReject the scenarios check.
type Report struct {
	MsgType     string
	ClOrdID     string
	OrigClOrdID string
	OrderID     string
	ExecType    string
	OrdStatus   string
	RejReason   string // OrdRejReason(103) or CxlRejReason(102)
	Text        string
	PossDup     bool
}

// Stats counts session-level events the client observed.
type Stats struct {
	Logons          int
	Logouts         int
	Heartbeats      int
	ResendRequests  int // ResendRequests received from the gateway
	PossDupResent   int // application messages this client re-sent with PossDupFlag=Y
	SequenceResets  int // SequenceReset (gap fill) messages this client sent
	AdminRejections int
}

// Client is a running initiator.
type Client struct {
	cfg  Config
	log  *slog.Logger
	sid  quickfix.SessionID
	init *quickfix.Initiator

	mu       sync.Mutex
	loggedOn bool
	reports  []Report
	stats    Stats
}

// New builds, but does not start, a client.
func New(cfg Config) (*Client, error) {
	if cfg.HeartBtInt == 0 {
		cfg.HeartBtInt = 30
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	c := &Client{
		cfg: cfg, log: cfg.Logger,
		sid: quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: cfg.SenderCompID, TargetCompID: cfg.TargetCompID},
	}
	settings, err := quickfix.ParseSettings(strings.NewReader(c.settingsText()))
	if err != nil {
		return nil, err
	}
	var store quickfix.MessageStoreFactory = quickfix.NewMemoryStoreFactory()
	if cfg.StoreDir != "" {
		store = filestore.NewStoreFactory(settings)
	}
	c.init, err = quickfix.NewInitiator(c, store, settings, quiet{})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) settingsText() string {
	yn := func(b bool) string {
		if b {
			return "Y"
		}
		return "N"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[DEFAULT]\nConnectionType=initiator\nReconnectInterval=1\nLogonTimeout=5\n")
	fmt.Fprintf(&b, "StartTime=00:00:00\nEndTime=00:00:00\nHeartBtInt=%d\n", c.cfg.HeartBtInt)
	fmt.Fprintf(&b, "SocketConnectHost=%s\nSocketConnectPort=%s\n", c.cfg.Host, c.cfg.Port)
	fmt.Fprintf(&b, "DataDictionary=%s\nResetOnLogon=%s\n", filepath.ToSlash(c.cfg.DictPath), yn(c.cfg.Reset))
	if c.cfg.StoreDir != "" {
		fmt.Fprintf(&b, "FileStorePath=%s\n", filepath.ToSlash(c.cfg.StoreDir))
	}
	fmt.Fprintf(&b, "[SESSION]\nBeginString=FIX.4.4\nSenderCompID=%s\nTargetCompID=%s\n", c.cfg.SenderCompID, c.cfg.TargetCompID)
	return b.String()
}

func (c *Client) Start() error { return c.init.Start() }
func (c *Client) Stop()        { c.init.Stop() }

// quickfix.Application ---------------------------------------------------

func (c *Client) OnCreate(quickfix.SessionID) {}

func (c *Client) OnLogon(quickfix.SessionID) {
	c.mu.Lock()
	c.loggedOn = true
	c.stats.Logons++
	c.mu.Unlock()
}

func (c *Client) OnLogout(quickfix.SessionID) {
	c.mu.Lock()
	c.loggedOn = false
	c.stats.Logouts++
	c.mu.Unlock()
}

// ToAdmin signs outgoing Logons.
func (c *Client) ToAdmin(m *quickfix.Message, _ quickfix.SessionID) {
	switch fixmsg.Str(&m.Header, fixmsg.TagMsgType) {
	case fixmsg.MsgLogon:
		sendingTime := fixmsg.Str(&m.Header, fixmsg.TagSendingTime)
		seq := fixmsg.Int(&m.Header, fixmsg.TagMsgSeqNum)
		sig := auth.Sign(c.cfg.Secret,
			auth.Payload(sendingTime, fixmsg.MsgLogon, seq, c.cfg.SenderCompID, c.cfg.TargetCompID, c.cfg.Password))
		m.Body.SetString(fixmsg.TagUsername, c.cfg.Username)
		m.Body.SetString(fixmsg.TagPassword, c.cfg.Password)
		m.Body.SetInt(fixmsg.TagRawDataLength, len(sig))
		m.Body.SetString(fixmsg.TagRawData, sig)
	case fixmsg.MsgSequenceReset:
		c.mu.Lock()
		c.stats.SequenceResets++
		c.mu.Unlock()
	}
}

func (c *Client) ToApp(m *quickfix.Message, _ quickfix.SessionID) error {
	if fixmsg.Str(&m.Header, fixmsg.TagPossDupFlag) == "Y" {
		c.mu.Lock()
		c.stats.PossDupResent++
		c.mu.Unlock()
	}
	return nil
}

func (c *Client) FromAdmin(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch fixmsg.Str(&m.Header, fixmsg.TagMsgType) {
	case fixmsg.MsgHeartbeat:
		c.stats.Heartbeats++
	case fixmsg.MsgResendRequest:
		c.stats.ResendRequests++
	case fixmsg.MsgReject:
		c.stats.AdminRejections++
	}
	return nil
}

func (c *Client) FromApp(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	mt := fixmsg.Str(&m.Header, fixmsg.TagMsgType)
	if mt != fixmsg.MsgExecutionReport && mt != fixmsg.MsgOrderCancelReject {
		return nil
	}
	r := Report{
		MsgType:     mt,
		ClOrdID:     fixmsg.Str(&m.Body, fixmsg.TagClOrdID),
		OrigClOrdID: fixmsg.Str(&m.Body, fixmsg.TagOrigClOrdID),
		OrderID:     fixmsg.Str(&m.Body, fixmsg.TagOrderID),
		ExecType:    fixmsg.Str(&m.Body, fixmsg.TagExecType),
		OrdStatus:   fixmsg.Str(&m.Body, fixmsg.TagOrdStatus),
		Text:        fixmsg.Str(&m.Body, fixmsg.TagText),
		PossDup:     fixmsg.Str(&m.Header, fixmsg.TagPossDupFlag) == "Y",
	}
	if mt == fixmsg.MsgOrderCancelReject {
		r.RejReason = fixmsg.Str(&m.Body, fixmsg.TagCxlRejReason)
	} else {
		r.RejReason = fixmsg.Str(&m.Body, fixmsg.TagOrdRejReason)
	}
	c.mu.Lock()
	c.reports = append(c.reports, r)
	c.mu.Unlock()
	return nil
}

// Sending -----------------------------------------------------------------

// SendOrder sends a NewOrderSingle.
func (c *Client) SendOrder(o fixmsg.NewOrder) error {
	return quickfix.SendToTarget(fixmsg.NewOrderSingle(o, time.Now()), c.sid)
}

// SendCancel sends an OrderCancelRequest.
func (c *Client) SendCancel(origClOrdID, clOrdID, symbol string, side oms.Side, qty string) error {
	return quickfix.SendToTarget(fixmsg.OrderCancelRequest(origClOrdID, clOrdID, symbol, side, qty, time.Now()), c.sid)
}

// Observing ---------------------------------------------------------------

func (c *Client) LoggedOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedOn
}

func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Reports returns a copy of everything received so far.
func (c *Client) Reports() []Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Report(nil), c.reports...)
}

// Find returns the first report matching pred.
func (c *Client) Find(pred func(Report) bool) (Report, bool) {
	for _, r := range c.Reports() {
		if pred(r) {
			return r, true
		}
	}
	return Report{}, false
}

var ErrTimeout = errors.New("simclient: timed out")

// WaitFor polls cond until it is true, ctx ends or timeout elapses.
func WaitFor(ctx context.Context, timeout time.Duration, cond func() bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrTimeout
		case <-tick.C:
		}
	}
}

// WaitLogon blocks until the session is logged on.
func (c *Client) WaitLogon(ctx context.Context, timeout time.Duration) error {
	return WaitFor(ctx, timeout, c.LoggedOn)
}

// WaitAck blocks until an ExecutionReport with the given ClOrdID and ExecType arrives.
func (c *Client) WaitAck(ctx context.Context, timeout time.Duration, clOrdID string, execType oms.ExecType) (Report, error) {
	want := string(rune(execType))
	var got Report
	err := WaitFor(ctx, timeout, func() bool {
		r, ok := c.Find(func(r Report) bool {
			return r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == clOrdID && r.ExecType == want
		})
		got = r
		return ok
	})
	return got, err
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// quiet is a LogFactory that drops everything; the client's own record is Reports and Stats.
type quiet struct{}

func (quiet) Create() (quickfix.Log, error)                             { return quietLog{}, nil }
func (quiet) CreateSessionLog(quickfix.SessionID) (quickfix.Log, error) { return quietLog{}, nil }

type quietLog struct{}

func (quietLog) OnIncoming([]byte)               {}
func (quietLog) OnOutgoing([]byte)               {}
func (quietLog) OnEvent(string)                  {}
func (quietLog) OnEventf(string, ...interface{}) {}
