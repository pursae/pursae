package iyzico

import "wpgo/internal/pay"

// codes maps iyzico's payment error codes (the ones its sandbox test cards return, after
// better-payment's error-codes.ts) to normalized codes. API validation codes are not listed and
// resolve to CodeUnknown with the original kept in ProviderCode.
var codes = map[string]pay.Code{
	"10051": pay.CodeInsufficientFunds, // not sufficient funds
	"10005": pay.CodeCardDeclined,      // do not honour
	"10012": pay.CodeCardDeclined,      // invalid transaction
	"10041": pay.CodeStolenOrLost,      // lost card
	"10043": pay.CodeStolenOrLost,      // stolen card
	"10054": pay.CodeExpiredCard,       // expired card
	"10084": pay.CodeInvalidCard,       // invalid CVC2
	"10057": pay.CodeCardDeclined,      // not permitted to cardholder
	"10058": pay.CodeConfiguration,     // not permitted to terminal
	"10034": pay.CodeFraudSuspected,    // fraud suspect
	"10093": pay.CodeCardDeclined,      // card blocked for online payments
	"10201": pay.CodeCardDeclined,      // card not permitted
	"10204": pay.CodeUnknown,           // general payment error
	"10206": pay.CodeInvalidCard,       // invalid CVC length
	"10207": pay.CodeCardDeclined,      // refer to issuer
	"10208": pay.CodeConfiguration,     // invalid merchant category code
	"10209": pay.CodeCardDeclined,      // blocked card
	"10210": pay.Code3DSFailed,         // invalid CAVV
	"10211": pay.Code3DSFailed,         // invalid ECI
	"10213": pay.CodeInvalidCard,       // BIN not found
	"10214": pay.CodeProviderDown,      // communication or system error
	"10215": pay.CodeInvalidCard,       // invalid card number
	"10216": pay.CodeInvalidCard,       // no such issuer
	"10217": pay.CodeCardDeclined,      // debit cards require 3D Secure
	"10219": pay.CodeProviderDown,      // request to the bank timed out
	"10222": pay.CodeInstallment,       // terminal not configured for installments
	"10223": pay.CodeProviderDown,      // end of day processing required
	"10225": pay.CodeCardDeclined,      // restricted card
	"10226": pay.CodeLimitExceeded,     // PIN tries exceeded
	"10227": pay.CodeCardDeclined,      // invalid PIN
	"10228": pay.CodeProviderDown,      // issuer or switch inoperative
	"10229": pay.CodeExpiredCard,       // invalid expiry date
	"10232": pay.CodeInvalidRequest,    // invalid amount
}

func mapCode(c string) pay.Code {
	if c == "" {
		return pay.CodeUnknown
	}
	if v, ok := codes[c]; ok {
		return v
	}
	return pay.CodeUnknown
}
