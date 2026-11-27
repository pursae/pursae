package pay

import "fmt"

// Code is a provider-independent reason for a failed payment. A provider maps its own codes to these
// and keeps the original in ProviderCode; what it cannot map is CodeUnknown.
type Code string

const (
	CodeNone              Code = ""
	CodeUnknown           Code = "unknown"
	CodeCardDeclined      Code = "card_declined"
	CodeInsufficientFunds Code = "insufficient_funds"
	CodeExpiredCard       Code = "expired_card"
	CodeInvalidCard       Code = "invalid_card" // number, CVV or expiry rejected
	CodeLimitExceeded     Code = "limit_exceeded"
	CodeStolenOrLost      Code = "stolen_or_lost"
	CodeFraudSuspected    Code = "fraud_suspected"
	Code3DSFailed         Code = "threeds_failed"
	CodeInstallment       Code = "installment_not_allowed"
	CodeCancelledByUser   Code = "cancelled_by_user"
	CodeTimeout           Code = "timeout" // the payment page expired
	CodeInvalidRequest    Code = "invalid_request"
	CodeConfiguration     Code = "configuration" // wrong credentials, terminal not enabled for this mode
	CodeDuplicate         Code = "duplicate"     // the provider already has this order id
	CodeNotFound          Code = "not_found"
	CodeProviderDown      Code = "provider_unavailable"
)

// Message is the text shown to the customer for a failure code (English; the store translates it).
// It never repeats provider text, which may name internal details.
func Message(c Code) string {
	switch c {
	case CodeCardDeclined:
		return "Your card was declined. Please try another card or contact your bank."
	case CodeInsufficientFunds:
		return "Your card has insufficient funds."
	case CodeExpiredCard:
		return "Your card has expired."
	case CodeInvalidCard:
		return "The card details are not valid. Please check them and try again."
	case CodeLimitExceeded:
		return "The payment exceeds your card's limit."
	case CodeStolenOrLost, CodeFraudSuspected:
		return "The payment was not approved. Please contact your bank."
	case Code3DSFailed:
		return "The 3D Secure verification failed. Please try again."
	case CodeInstallment:
		return "This card does not allow the selected installment plan."
	case CodeCancelledByUser:
		return "The payment was cancelled."
	case CodeTimeout:
		return "The payment page expired. Please try again."
	case CodeProviderDown:
		return "The payment service is not available right now. Please try again in a few minutes."
	}
	return "The payment could not be completed. Please try again or choose another payment method."
}

// Error is a provider's rejection of a request (Start, Refund, Query).
type Error struct {
	Code            Code
	ProviderCode    string
	ProviderMessage string
}

func (e *Error) Error() string {
	if e.ProviderCode == "" && e.ProviderMessage == "" {
		return fmt.Sprintf("pay: %s", e.Code)
	}
	return fmt.Sprintf("pay: %s (provider: %s %s)", e.Code, e.ProviderCode, e.ProviderMessage)
}

// Errorf builds an *Error.
func Errorf(code Code, providerCode, format string, a ...any) *Error {
	return &Error{Code: code, ProviderCode: providerCode, ProviderMessage: fmt.Sprintf(format, a...)}
}
