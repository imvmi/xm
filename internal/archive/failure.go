package archive

import "errors"

// Failure identifies conditions requiring scheduler action, without inspecting
// localized error text or exposing raw upstream responses.
type Failure struct {
	Kind    string
	Message string
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
