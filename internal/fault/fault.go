// Package fault separates readable failure messages from diagnostic causes.
package fault

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type failure struct {
	message string
	cause   error
}

func (e *failure) Error() string { return Redact(e.message) }
func (e *failure) Unwrap() error { return e.cause }

// Wrap preserves errors.Is/As while keeping transport details out of the UI.
func Wrap(message string, cause error) error { return &failure{message, cause} }

type actionRequired struct{ error }

func (e *actionRequired) Unwrap() error { return e.error }

// Action marks a local failure that needs attention rather than a network retry.
func Action(message string, cause error) error {
	return &actionRequired{Wrap(message, cause)}
}

func RequiresAction(err error) bool {
	var action *actionRequired
	return errors.As(err, &action)
}

// Reason is a short explanation. Specific operation messages take precedence
// over generic timeout/EOF labels; a timeout never implies a broadcast ended.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	var action *actionRequired
	if errors.As(err, &action) {
		err = action.error
	}
	var described *failure
	if errors.As(err, &described) {
		return Redact(described.message)
	}
	if errors.Is(err, context.Canceled) {
		return "the operation was canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return "the operation timed out"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "the connection closed before more data arrived"
	}
	var transport *net.OpError
	if errors.As(err, &transport) {
		return "the network connection failed"
	}
	var request *url.Error
	if errors.As(err, &request) {
		return "the network request failed"
	}
	var file *os.PathError
	if errors.As(err, &file) {
		return "the file operation failed: " + file.Err.Error()
	}
	return Redact(strings.ReplaceAll(err.Error(), "\n", "; "))
}

// Details is intended for the local diagnostic log, never the main UI message.
func Details(err error) string {
	return Redact(details(err))
}

func details(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(interface{ DiagnosticDetails() string }); ok {
		return e.DiagnosticDetails()
	}
	if e, ok := err.(*failure); ok {
		if e.cause == nil {
			return e.message
		}
		return e.message + ": " + Details(e.cause)
	}
	if e, ok := err.(interface{ Unwrap() []error }); ok {
		var details []string
		for _, cause := range e.Unwrap() {
			details = append(details, Details(cause))
		}
		return strings.Join(details, "\n")
	}
	if e, ok := err.(*actionRequired); ok {
		return Details(e.error)
	}
	if cause := errors.Unwrap(err); cause != nil {
		// fmt.Errorf may already contain a readable wrapper's message. Keep
		// that operation and append the deeper cause only when it adds detail.
		detail := Details(cause)
		if strings.Contains(err.Error(), detail) {
			return err.Error()
		}
		return err.Error() + ": " + detail
	}
	return err.Error()
}

var links = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
var cookies = regexp.MustCompile(`(?i)\b(sessionid(?:_ss)?|sid_tt|sid_guard|uid_tt(?:_ss)?|tt_chain_token|msToken|passport_csrf_token)\s*=\s*[^;\s]+`)

// Redact never retains signed request/media URLs or known session cookie values.
// HTTP errors may include a URL even when request headers were never logged.
func Redact(text string) string {
	text = links.ReplaceAllString(text, "[URL omitted]")
	return cookies.ReplaceAllString(text, "$1=[omitted]")
}
