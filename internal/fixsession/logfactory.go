package fixsession

import (
	"fmt"
	"log/slog"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/audit"
)

// auditLogFactory is a quickfix.LogFactory that sends every raw message to the
// audit log and session events to slog. Hooking in here, rather than in the
// Application callbacks, means the audit trail also covers messages quickfix
// handles itself (heartbeats, resends, rejects).
type auditLogFactory struct {
	audit  *audit.Logger // may be nil
	logger *slog.Logger
}

// NewLogFactory returns a LogFactory writing to a (optional) and logger.
func NewLogFactory(a *audit.Logger, logger *slog.Logger) quickfix.LogFactory {
	return &auditLogFactory{audit: a, logger: logger}
}

func (f *auditLogFactory) Create() (quickfix.Log, error) {
	return &sessionLog{f: f, name: "global"}, nil
}

func (f *auditLogFactory) CreateSessionLog(sid quickfix.SessionID) (quickfix.Log, error) {
	return &sessionLog{f: f, name: sid.String()}, nil
}

type sessionLog struct {
	f    *auditLogFactory
	name string
}

func (l *sessionLog) OnIncoming(b []byte) {
	if l.f.audit != nil {
		l.f.audit.Log("in", l.name, b)
	}
}

func (l *sessionLog) OnOutgoing(b []byte) {
	if l.f.audit != nil {
		l.f.audit.Log("out", l.name, b)
	}
}

func (l *sessionLog) OnEvent(s string) {
	l.f.logger.Info("fix event", "session", l.name, "event", s)
}

func (l *sessionLog) OnEventf(format string, a ...interface{}) {
	l.OnEvent(fmt.Sprintf(format, a...))
}
