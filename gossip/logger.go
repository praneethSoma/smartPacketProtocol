package gossip

// ──────────────────────────────────────────────────────────────
// Logger — gossip package uses the same pattern as packet/.
// ──────────────────────────────────────────────────────────────

// Logger defines the logging interface for the gossip package.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}

// log is the package-level logger. Replace with SetLogger().
var log Logger = noopLogger{}

// SetLogger replaces the package-level logger for gossip.
func SetLogger(l Logger) {
	if l == nil {
		log = noopLogger{}
		return
	}
	log = l
}
