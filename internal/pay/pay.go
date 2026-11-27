// Package pay is the provider layer of Pursae Commerce's hosted payments: one contract for payment
// service providers and bank virtual POS systems (PayTR, iyzico, the EST banks, PayPal, …).
//
// A provider package (internal/pay/<name>) knows one provider's protocol: which fields to send, how to
// sign them, how to verify what comes back, how to refund. It knows nothing about Pursae: no database,
// no orders, no HTTP routing. The Commerce bridge (internal/server, goo_gateway_pay.go) does that part
// once for every provider: it records each payment attempt, routes callbacks, deduplicates
// notifications, compares the paid amount with the attempt and moves the order.
//
// Hosted only. The card number is typed on the provider's page or inside the provider's iframe and
// never reaches this server. Nothing in this package accepts, carries or logs card data, and a provider
// package must not add a mode that does.
//
// The protocols were written with the MIT-licensed better-payment library
// (github.com/czaydev/better-payment) as the reference for field names and signatures; see NOTICE.
package pay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Status is the outcome of an operation, the same four values for every provider.
type Status string

const (
	// StatusSuccess: the provider confirmed the operation (signed data or a server-side lookup).
	StatusSuccess Status = "success"
	// StatusFailure: the provider rejected it; see Result.Code, ProviderCode and ProviderMessage.
	StatusFailure Status = "failure"
	// StatusPending: no outcome yet (the customer is still paying, or this callback carries no proof).
	StatusPending Status = "pending"
	// StatusCancelled: the payment was voided or fully refunded (status queries).
	StatusCancelled Status = "cancelled"
)

// Region groups providers in Settings › Payments.
type Region string

const (
	RegionTR     Region = "tr"     // Türkiye
	RegionGlobal Region = "global" // Worldwide
)

// Kind is the type of provider inside a region.
type Kind string

const (
	KindPSP    Kind = "psp"    // payment service provider (PayTR, iyzico, Param, Sipay)
	KindBank   Kind = "bank"   // a bank's virtual POS
	KindWallet Kind = "wallet" // wallet / account payments (PayPal)
)

// Feature is something a provider can do, shown as a label in Settings › Payments.
type Feature string

const (
	Feature3DS          Feature = "3ds"
	FeatureInstallments Feature = "installments"
	FeatureRefunds      Feature = "refunds"
	FeatureQuery        Feature = "query" // Query is implemented
)

// Field is one setting of a provider (a credential or an option). The bridge adds the common fields
// itself (enabled, title, description, test mode); a Spec lists only what is specific to the provider.
type Field struct {
	ID       string   // key in Config.Values: lowercase, [a-z0-9_]
	Title    string   // label, English
	Type     string   // "text", "password", "select", "checkbox", "number"
	Default  string   // checkbox: "yes" / "no"
	Desc     string   // help text, plain text
	Options  []Option // select choices
	Secret   bool     // a credential: never printed back into the settings page, masked in logs
	Required bool     // the gateway cannot be enabled while this is empty
}

// Option is a select choice.
type Option struct{ Value, Label string }

// Hosts are the origins a provider's payment step needs in the Content-Security-Policy of the page the
// bridge renders: "https://www.paytr.com". Only what StartResult really uses.
type Hosts struct {
	Frame  []string // frame-src: StartResult.Embed.FrameURL
	Script []string // script-src: StartResult.Embed.Scripts
	Form   []string // form-action: StartResult.Form.Action
}

// Spec describes a provider. Each provider package exports func Spec() pay.Spec.
type Spec struct {
	ID          string    // gateway id: lowercase [a-z0-9_], e.g. "paytr", "est_isbank"
	Title       string    // "PayTR"
	Description string    // one sentence for Settings › Payments, English
	Region      Region    // tr / global
	Kind        Kind      // psp / bank / wallet
	Features    []Feature // what this implementation supports
	Currencies  []string  // ISO 4217 codes the provider can charge; empty: any
	Fields      []Field   // credentials and options
	Hosts       Hosts     // CSP origins of the payment step, live and test
	DocsURL     string    // the provider's integration documentation
	// New builds a provider from saved settings. It must not do network I/O. An error means the
	// settings are incomplete or invalid (the message is shown to the store admin).
	New func(Config) (Provider, error)
}

// Config is what a provider is built from.
type Config struct {
	Values map[string]string // Spec.Fields by ID (checkbox: "yes" / "no"), surrounding space trimmed
	Test   bool              // test / sandbox mode: the provider's test endpoints and test flags
	HTTP   *http.Client      // outbound client with a timeout; never nil
	// BaseURL replaces the provider's API origin. For tests only (httptest.Server): the bridge never
	// sets it, and it is not a setting, so a store admin cannot point a provider at another host.
	BaseURL string
}

// Provider is one configured provider. Implementations are safe for concurrent use.
type Provider interface {
	// Start begins a hosted payment for an attempt. Nothing is charged yet. An error means the payment
	// could not be started (the customer may retry or pick another method); wrap provider rejections
	// in *Error so the bridge can show a useful message.
	Start(ctx context.Context, r StartRequest) (*StartResult, error)

	// CallbackRef returns the attempt reference (StartRequest.Ref) an incoming callback is about, read
	// from the callback without trusting it, or "" when it names none. The bridge uses it to load the
	// attempt and then calls Complete, which does the verification.
	CallbackRef(in CallbackInput) string

	// Complete verifies a callback against the stored attempt and reports the outcome.
	//
	//   - It returns an error (ErrInvalidSignature, or any other) when the callback fails verification
	//     or is not about this attempt. The bridge then answers 400 and changes nothing.
	//   - It returns StatusSuccess only when the payment is proven: a signature made with the store's
	//     secret over data that binds this attempt's reference, or the provider's answer to a
	//     server-side request made here (retrieve, capture, provision). A browser landing without proof
	//     is StatusPending, never success.
	//   - On success Result.PaidAmount is the amount the proof covers, in minor units, and
	//     Result.Currency its currency when the proof names one. The bridge refuses to mark an order
	//     paid when PaidAmount is 0 or below Attempt.Amount, or the currency differs.
	//   - When the provider expects a specific response body to a notification, Result.Ack carries it;
	//     the bridge sends it only after the outcome is stored.
	//
	// Complete may be called more than once for the same attempt (a notification and a browser return,
	// provider retries). Server-side requests it makes must be safe to repeat or guarded by the
	// provider's own idempotency.
	Complete(ctx context.Context, in CallbackInput, a Attempt) (*Result, error)

	// Query asks the provider for the current state of an attempt. ErrNotSupported when the provider
	// has no status API.
	Query(ctx context.Context, a Attempt) (*Result, error)

	// Refund returns money for a paid attempt. The provider decides between a void (same-day cancel)
	// and a refund from RefundRequest.Full and its own rules. It is never retried automatically: when
	// the outcome is unknown (timeout, connection lost) it returns ErrUnknownOutcome.
	Refund(ctx context.Context, r RefundRequest) (*Result, error)
}

// StartRequest is one payment attempt.
type StartRequest struct {
	// Ref is the attempt reference, unique per attempt: 8–32 characters of [A-Za-z0-9]. Send it as the
	// provider's order id / merchant_oid / conversation id; it is how callbacks find the attempt.
	Ref string
	// OrderNumber is the store's order number, for descriptions shown to the customer.
	OrderNumber string
	// Amount is the total to charge in minor units (kuruş, cents).
	Amount int64
	// Currency is the ISO 4217 code: "TRY", "USD", "EUR".
	Currency string
	// Installment is the number of installments chosen in the store for providers whose hosted page
	// does not ask (bank POS): 0 or 1 is a single payment.
	Installment int
	// MaxInstallment limits what the provider's own page offers (PSPs): 0 the provider's limit, 1 no
	// installments.
	MaxInstallment int
	// Description is a short text for the payment: "Order #1042 – Store name".
	Description string
	Customer    Customer
	Billing     Address
	Shipping    Address // equals Billing when the order has no shipping address
	// Items sum to Amount exactly (Quantity × UnitPrice): the bridge adds shipping and fees as items
	// and spreads discounts, so providers that validate the basket total accept it. Never empty.
	Items []Item
	// ReturnURL is where the customer's browser comes back after paying (the provider's success /
	// callback / ok URL). FailURL is the failure counterpart for providers that take two URLs.
	// NotifyURL is for the provider's server-to-server notification. All are absolute HTTPS URLs on
	// this store and already carry what the bridge needs to route them; append nothing but what the
	// provider's protocol requires.
	ReturnURL string
	FailURL   string
	NotifyURL string
	// Locale is the customer's language: "tr" or "en".
	Locale string
	// ExpiresIn is how long the payment page should stay valid, for providers that take a timeout.
	ExpiresIn time.Duration
}

// Customer is the buyer.
type Customer struct {
	ID        string // store customer id, "" for a guest
	Email     string
	FirstName string
	LastName  string
	Phone     string
	IP        string // the customer's IP address as the store sees it
}

// Address is a billing or shipping address.
type Address struct {
	Name     string // contact name
	Line1    string
	Line2    string
	City     string
	State    string
	Postcode string
	Country  string // ISO 3166-1 alpha-2: "TR"
}

// Item is a basket line.
type Item struct {
	ID        string // product id / SKU, or "shipping", "fee"
	Name      string
	Category  string
	Quantity  int
	UnitPrice int64 // minor units, tax included
	Virtual   bool  // not shipped (digital goods, shipping and fee lines)
}

// StartResult tells the bridge how to send the customer to the provider. Exactly one of Redirect, Form
// and Embed is set. None of them contains a card field.
type StartResult struct {
	// Redirect is a URL on the provider's site to send the browser to (GET).
	Redirect string
	// Form is a form of hidden fields the bridge auto-submits (POST) to the provider's hosted page.
	Form *Form
	// Embed is a provider iframe shown inside a page of the store.
	Embed *Embed
	// ProviderRef is the provider's own id for this attempt (token, order id), when it has one. The
	// bridge stores it and hands it back in Attempt.ProviderRef.
	ProviderRef string
}

// Form is an auto-submitted POST to the provider.
type Form struct {
	Action string      // absolute HTTPS URL, an origin listed in Spec.Hosts.Form
	Fields [][2]string // name, value in order
}

// Embed is a provider iframe.
type Embed struct {
	FrameURL string   // iframe src, an origin listed in Spec.Hosts.Frame
	Scripts  []string // provider scripts to load, origins listed in Spec.Hosts.Script
	// Init is JavaScript run after Scripts have loaded. It is written in the provider package (never
	// built from provider or customer data); the iframe element has the id FrameID.
	Init string
}

// FrameID is the DOM id of the iframe the bridge renders for an Embed.
const FrameID = "goo-pay-frame"

// CallbackKind says which of the URLs of a StartRequest was called.
type CallbackKind string

const (
	CallbackReturn CallbackKind = "return" // ReturnURL, the customer's browser
	CallbackFail   CallbackKind = "fail"   // FailURL, the customer's browser
	CallbackNotify CallbackKind = "notify" // NotifyURL, the provider's server
)

// CallbackInput is an incoming request on one of the attempt's URLs. Everything in it is untrusted
// until Complete has verified it.
type CallbackInput struct {
	Kind   CallbackKind
	Method string
	Query  url.Values  // URL query, without the bridge's own routing parameters
	Form   url.Values  // parsed body of a form post (application/x-www-form-urlencoded, multipart)
	Header http.Header // request headers
	Body   []byte      // raw body (JSON / XML notifications), at most 1 MiB
}

// Attempt is what the bridge stored about a payment attempt when it was started.
type Attempt struct {
	Ref         string
	ProviderRef string // StartResult.ProviderRef
	Amount      int64  // minor units
	Currency    string
	Installment int
	Test        bool // the attempt was started in test mode
	CreatedAt   time.Time
	// Paid* are set once the attempt is paid (refunds, queries).
	PaidAmount int64
	TxnID      string
}

// Result is the outcome of Complete, Query and Refund.
type Result struct {
	Status Status
	// TxnID is the provider's transaction id (payment id, auth code reference), kept on the order.
	TxnID string
	// PaidAmount is the amount charged (or refunded, for Refund) in minor units; 0 when unknown. It can
	// exceed Attempt.Amount when the provider added installment interest.
	PaidAmount int64
	// Currency is the ISO 4217 code the proof names, "" when it names none.
	Currency string
	// Installment is the number of installments the customer chose, 0 when unknown.
	Installment int
	// Code is the normalized reason of a failure; ProviderCode and ProviderMessage are the provider's.
	Code            Code
	ProviderCode    string
	ProviderMessage string
	// Ack is the response the provider expects to a notification.
	Ack *Ack
}

// Ack is an HTTP response for a provider notification.
type Ack struct {
	Status      int // 0: 200
	ContentType string
	Body        string
}

// RefundRequest asks for money back on a paid attempt.
type RefundRequest struct {
	Attempt Attempt
	Amount  int64 // minor units to return
	// Full is true when Amount is the whole paid amount and nothing was refunded before: providers
	// that distinguish use a void / cancel when the payment has not settled yet.
	Full bool
	// RefundRef identifies this refund, unique per refund: [A-Za-z0-9], at most 32 characters. Send it
	// as the provider's idempotency key or refund reference where one exists.
	RefundRef string
	Reason    string
}

var (
	// ErrInvalidSignature: a callback's signature or hash does not match.
	ErrInvalidSignature = errors.New("pay: invalid callback signature")
	// ErrNotSupported: the provider has no such operation.
	ErrNotSupported = errors.New("pay: not supported by this provider")
	// ErrUnknownOutcome: the request may or may not have been processed (timeout, connection lost).
	// The caller must check with Query, or in the provider's panel, before trying again.
	ErrUnknownOutcome = errors.New("pay: outcome unknown, check the payment before retrying")
)
