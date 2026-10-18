package archive

import (
	"errors"
	"time"
)

// Failure identifies conditions requiring scheduler action, without inspecting
// localized error text or exposing raw upstream responses.
type Failure struct {
	Kind    string
	Message string
	RetryAt time.Time
}

func (e *Failure) Error() string      { return e.Message }
func fail(kind, message string) error { return &Failure{Kind: kind, Message: message} }
func FailureKind(err error) string {
	var e *Failure
	if errors.As(err, &e) {
		return e.Kind
	}
	return "transient"
}
