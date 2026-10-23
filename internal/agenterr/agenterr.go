// Package agenterr holds the agent's error types: one struct with a Type name, plus Is*
// helpers: AgentToolCallError and AgentToolExecutionError are AgentExecutionErrors, all are AgentErrors.
package agenterr

import (
	"errors"

	"canvas_go/internal/logger"
)

const (
	TypeAgentError              = "AgentError"
	TypeAgentParsingError       = "AgentParsingError"
	TypeAgentExecutionError     = "AgentExecutionError"
	TypeAgentMaxStepsError      = "AgentMaxStepsError"
	TypeAgentToolCallError      = "AgentToolCallError"
	TypeAgentToolExecutionError = "AgentToolExecutionError"
	TypeAgentGenerationError    = "AgentGenerationError"
)

// AgentError is any error raised by the agent loop. Creating one logs it.
type AgentError struct {
	Type    string
	Message string
	Cause   error
}

func (e *AgentError) Error() string { return e.Message }
func (e *AgentError) Unwrap() error { return e.Cause }

// AsMap is the JSON form stored with a step.
func (e *AgentError) AsMap() map[string]string {
	return map[string]string{"type": e.Type, "message": e.Message}
}

// New creates (and logs) an agent error of the given type.
func New(typ, message string, l *logger.AgentLogger) *AgentError {
	if l != nil {
		l.LogError(message)
	}
	return &AgentError{Type: typ, Message: message}
}

// Wrap is New with the underlying cause kept for errors.Is/As.
func Wrap(typ, message string, cause error, l *logger.AgentLogger) *AgentError {
	e := New(typ, message, l)
	e.Cause = cause
	return e
}

// As returns the AgentError in err's chain, if any.
func As(err error) (*AgentError, bool) {
	var e *AgentError
	ok := errors.As(err, &e)
	return e, ok
}

// IsType reports whether err is an AgentError of exactly this type.
func IsType(err error, typ string) bool {
	e, ok := As(err)
	return ok && e.Type == typ
}
