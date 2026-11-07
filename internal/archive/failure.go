package archive

import (
	"errors"
	"time"
)

// Failure identifies conditions requiring scheduler action, without inspecting
// localized error text or exposing raw upstream responses.
type Failure struct {
	Kind    string
	Reason  string
	Message string
	RetryAt time.Time
}

const (
	AccountIdentityConflictMessage = "当前用户名对应的数字用户 ID 与原归档不同"
	AccountSuspendedMessage        = "X 已明确标记目标账号被冻结"
	AccountNotFoundMessage         = "X 未找到该账号，可能已改名、注销或被删除"
	AccountLookupMissingMessage    = "未找到该用户名对应的账号资料，可能已改名或不可访问"
	AccountProtectedMessage        = "当前采集账号无权查看该锁推账号的内容"
	AccountUnavailableMessage      = "目标账号不可用或当前账号无权访问，原因尚未确认"
)

func accountFailure(reason, message string) error {
	return &Failure{Kind: "access", Reason: reason, Message: message}
}

func FailureReason(err error) string {
	var e *Failure
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
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
