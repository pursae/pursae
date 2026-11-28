// Package est is the EST / Nestpay (Asseco) bank virtual POS, shared by several Turkish banks: one
// protocol, one Spec per bank with the bank's own hosts.
//
// Hosted store types only: in 3d_pay_hosting the bank shows its own card page, authenticates the card
// and charges it; in 3d_host the bank shows its card page and authenticates the card, and this package
// charges it with an XML API Auth that carries the bank's opaque md token (never a card number).
// The card is typed on the bank's page; no field of this package holds card data.
package est

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"wpgo/internal/pay"
)

const (
	gatePath = "/fim/est3Dgate" // 3D gate: the customer's browser posts the form here
	apiPath  = "/fim/api"       // XML API (CC5Request)

	// testOrigin is Asseco's shared EST test host, used by every bank in test mode.
	testOrigin = "https://entegrasyon.asseco-see.com.tr"

	storePayHosting = "3d_pay_hosting"
	storeHost       = "3d_host"
)

type bank struct {
	id, title, live string
}

// banks are the EST banks with their production hosts (better-payment PROVIDER_DEFAULT_URLS).
var banks = []bank{
	{"est_isbank", "İş Bankası", "https://sanalpos.isbank.com.tr"},
	{"est_ziraat", "Ziraat Bankası", "https://sanalpos2.ziraatbank.com.tr"},
	{"est_halkbank", "Halkbank", "https://sanalpos.halkbank.com.tr"},
	{"est_teb", "TEB", "https://sanalpos.teb.com.tr"},
	{"est_sekerbank", "Şekerbank", "https://sanalpos.sekerbank.com.tr"},
	{"est_akbank", "Akbank (legacy EST POS)", "https://www.sanalakpos.com"},
}

// Specs are the EST banks, one gateway each.
func Specs() []pay.Spec {
	out := make([]pay.Spec, 0, len(banks))
	for _, b := range banks {
		out = append(out, spec(b))
	}
	return out
}

func spec(b bank) pay.Spec {
	return pay.Spec{
		ID:          b.id,
		Title:       b.title,
		Description: b.title + " virtual POS (EST / Nestpay): the customer pays on the bank's 3D Secure card page.",
		Region:      pay.RegionTR,
		Kind:        pay.KindBank,
		Features:    []pay.Feature{pay.Feature3DS, pay.FeatureRefunds, pay.FeatureQuery, pay.FeatureInstallments},
		Currencies:  []string{"TRY", "USD", "EUR", "GBP"},
		Fields: []pay.Field{
			{ID: "client_id", Title: "Client ID (Mağaza No)", Type: "text", Required: true,
				Desc: "Merchant ID from the bank's merchant panel."},
			{ID: "store_key", Title: "Store key (3D key)", Type: "password", Secret: true, Required: true,
				Desc: "The 3D Secure store key set in the merchant panel; signs the payment form and the bank's answer."},
			{ID: "api_user", Title: "API user", Type: "text", Required: true,
				Desc: "API user created in the merchant panel; confirms payments with the bank and sends refunds."},
			{ID: "api_password", Title: "API password", Type: "password", Secret: true, Required: true},
			{ID: "store_type", Title: "Store type", Type: "select", Default: storePayHosting,
				Desc: "The 3D model the bank enabled for the store.",
				Options: []pay.Option{
					{Value: storePayHosting, Label: "3D Pay Hosting (the bank charges the card)"},
					{Value: storeHost, Label: "3D Host (the store charges after 3D Secure)"},
				}},
		},
		Hosts:   pay.Hosts{Form: []string{b.live, testOrigin}},
		DocsURL: "https://entegrasyon.asseco-see.com.tr/",
		New: func(c pay.Config) (pay.Provider, error) {
			p, err := newProvider(b, c)
			if err != nil {
				return nil, err // not a typed nil inside the interface
			}
			return p, nil
		},
	}
}

type provider struct {
	cfg       pay.Config
	clientID  string
	storeKey  string
	user      string
	password  string
	storeType string
	origin    string
}

func newProvider(b bank, c pay.Config) (*provider, error) {
	if err := c.Require("client_id", "store_key", "api_user", "api_password"); err != nil {
		return nil, err
	}
	st := c.Value("store_type")
	if st == "" {
		st = storePayHosting
	}
	if st != storePayHosting && st != storeHost {
		return nil, fmt.Errorf("pay: unknown EST store type %q", st)
	}
	return &provider{
		cfg:      c,
		clientID: c.Value("client_id"), storeKey: c.Value("store_key"),
		user: c.Value("api_user"), password: c.Value("api_password"),
		storeType: st,
		origin:    c.Origin(b.live, testOrigin),
	}, nil
}

// Start builds the form the customer's browser posts to the bank's 3D gate. The field set follows
// better-payment's initThreeDSPayment for the hosted store types (no card fields).
func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur := pay.NumericCurrency(r.Currency)
	if cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported", r.Currency)
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "amount must be positive")
	}
	failURL := r.FailURL
	if failURL == "" {
		failURL = r.ReturnURL
	}
	lang := "tr"
	if strings.HasPrefix(strings.ToLower(r.Locale), "en") {
		lang = "en"
	}
	rnd, err := random()
	if err != nil {
		return nil, err
	}
	fields := [][2]string{
		{"clientid", p.clientID},
		{"storetype", p.storeType},
		{"amount", pay.FormatMinor(r.Amount, r.Currency)},
		{"oid", r.Ref},
		{"okUrl", r.ReturnURL},
		{"failUrl", failURL},
		{"TranType", "Auth"},
		{"currency", cur},
	}
	if r.Installment > 1 {
		fields = append(fields, [2]string{"taksit", strconv.Itoa(r.Installment)})
	}
	fields = append(fields,
		[2]string{"rnd", rnd},
		[2]string{"lang", lang},
		[2]string{"hashAlgorithm", "ver3"},
		[2]string{"refreshTime", "0"},
	)
	if r.Customer.Email != "" {
		fields = append(fields, [2]string{"Email", r.Customer.Email})
	}
	fields = append(fields, [2]string{"hash", Hash(fields, p.storeKey)})
	return &pay.StartResult{Form: &pay.Form{Action: p.origin + gatePath, Fields: fields}}, nil
}

// CallbackRef reads the order id the bank posts back.
func (p *provider) CallbackRef(in pay.CallbackInput) string {
	for _, k := range []string{"oid", "ReturnOid"} {
		if v := strings.TrimSpace(in.Form.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// acceptedMD are the mdStatus values a payment may succeed with. 1 is full 3D Secure authentication;
// in 3d_pay_hosting the bank has already charged the card and also charges the half-secure 2, 3 and 4
// (card or issuer not enrolled, attempt), so those are accepted there too. 3d_host charges only after
// full authentication (better-payment's acceptedMdStatuses default ['1']).
func (p *provider) acceptedMD(md string) bool {
	if p.storeType == storeHost {
		return md == "1"
	}
	switch md {
	case "1", "2", "3", "4":
		return true
	}
	return false
}

// Complete verifies the bank's POST to okUrl / failUrl.
//
// The ver3 HASH covers the sorted values but not their field names, so a valid hash alone does not pin
// which value is the order id or the amount (better-payment, completeThreeDSPayment). Success is
// therefore confirmed with the bank as the reference does: in 3d_pay_hosting a payment needs a valid
// HASH, the store's clientid, oid equal to the attempt, an accepted mdStatus, Response "Approved" and
// ProcReturnCode "00", and then an order status query that shows a captured sale for the same amount;
// in 3d_host the XML Auth with md decides.
func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	f := firstValues(in.Form)
	if f["HASH"] == "" {
		return nil, pay.ErrInvalidSignature
	}
	if !Verify(f, p.storeKey) {
		return nil, pay.ErrInvalidSignature
	}
	if f["clientid"] != p.clientID {
		return nil, errors.New("pay: EST callback names another store")
	}
	oid := f["oid"]
	if oid == "" {
		oid = f["ReturnOid"]
	}
	if oid == "" || oid != a.Ref {
		return nil, errors.New("pay: EST callback is about another order")
	}

	md := f["mdStatus"]
	if !p.acceptedMD(md) {
		msg := f["mdErrorMsg"]
		if msg == "" {
			msg = f["ErrMsg"]
		}
		return &pay.Result{Status: pay.StatusFailure, Code: pay.Code3DSFailed,
			ProviderCode: "mdStatus " + md, ProviderMessage: msg}, nil
	}

	if p.storeType == storeHost {
		return p.charge(ctx, f, a)
	}

	if f["Response"] != "Approved" || f["ProcReturnCode"] != "00" {
		code := f["ProcReturnCode"]
		return &pay.Result{Status: pay.StatusFailure, Code: mapCode(code), ProviderCode: code,
			ProviderMessage: f["ErrMsg"]}, nil
	}
	currency := pay.CurrencyFromNumeric(f["currency"])
	if currency == "" {
		return nil, fmt.Errorf("pay: EST callback has an unknown currency %q", f["currency"])
	}
	paid, err := pay.ParseMinor(f["amount"], currency)
	if err != nil {
		return nil, err
	}
	st, err := p.orderStatus(ctx, oid)
	if err != nil {
		return nil, err
	}
	if !capturedSale(st) || (st.OrderID != "" && st.OrderID != oid) {
		return nil, errors.New("pay: EST callback does not match the order at the bank")
	}
	if c, ok := captureAmount(st, currency); ok && c != paid {
		return nil, errors.New("pay: EST callback amount differs from the order at the bank")
	}
	txn := f["TransId"]
	if txn == "" {
		txn = st.TransID
	}
	return &pay.Result{Status: pay.StatusSuccess, TxnID: txn, PaidAmount: paid, Currency: currency,
		Installment: installment(f["taksit"]), ProviderCode: f["ProcReturnCode"],
		ProviderMessage: authMessage(f["AuthCode"], f["HostRefNum"])}, nil
}

// charge completes a 3d_host payment: the bank authenticated the card and returned md, an opaque token
// standing for it; the XML Auth charges the attempt's own amount and currency. When the Auth is not
// approved the order status is asked, so a repeated Complete for an order already charged succeeds.
func (p *provider) charge(ctx context.Context, f map[string]string, a pay.Attempt) (*pay.Result, error) {
	cur := pay.NumericCurrency(a.Currency)
	if cur == "" || f["md"] == "" {
		return nil, errors.New("pay: EST callback cannot be charged")
	}
	taksit := ""
	if a.Installment > 1 {
		taksit = strconv.Itoa(a.Installment)
	}
	res, err := p.api(ctx, []kv{
		{"Type", "Auth"},
		{"OrderId", a.Ref},
		{"Total", pay.FormatMinor(a.Amount, a.Currency)},
		{"Currency", cur},
		{"Taksit", taksit},
		{"Number", f["md"]},
		{"PayerTxnId", f["xid"]},
		{"PayerSecurityLevel", f["eci"]},
		{"PayerAuthenticationCode", f["cavv"]},
		{"Mode", "P"},
	})
	if err != nil {
		return nil, err
	}
	if res.approved() && (res.OrderID == "" || res.OrderID == a.Ref) {
		return &pay.Result{Status: pay.StatusSuccess, TxnID: res.TransID, PaidAmount: a.Amount,
			Currency: a.Currency, Installment: a.Installment, ProviderCode: res.ProcReturnCode,
			ProviderMessage: authMessage(res.AuthCode, res.HostRefNum)}, nil
	}
	st, qerr := p.orderStatus(ctx, a.Ref)
	if qerr == nil && capturedSale(st) && (st.OrderID == "" || st.OrderID == a.Ref) {
		if c, ok := captureAmount(st, a.Currency); !ok || c == a.Amount {
			return &pay.Result{Status: pay.StatusSuccess, TxnID: st.TransID, PaidAmount: a.Amount,
				Currency: a.Currency, Installment: a.Installment, ProviderCode: st.ProcReturnCode}, nil
		}
	}
	return &pay.Result{Status: pay.StatusFailure, Code: mapCode(res.ProcReturnCode),
		ProviderCode: res.ProcReturnCode, ProviderMessage: res.ErrMsg}, nil
}

// Query asks the bank for the order status (ORDERSTATUS) of the attempt.
func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	st, err := p.orderStatus(ctx, a.Ref)
	if err != nil {
		return nil, err
	}
	r := &pay.Result{TxnID: st.TransID, Currency: a.Currency, ProviderCode: st.ProcReturnCode}
	switch {
	case st.Extra["TRANS_STAT"] == "":
		r.Status, r.Code, r.ProviderMessage = pay.StatusFailure, pay.CodeNotFound, st.ErrMsg
	case st.Extra["TRANS_STAT"] == "V", st.Extra["CHARGE_TYPE_CD"] == "C":
		// Voided, or the last approved transaction is a (partial) refund.
		r.Status = pay.StatusCancelled
	case st.Extra["TRANS_STAT"] == "D":
		code := st.Extra["PROC_RET_CD"]
		r.Status, r.Code, r.ProviderCode, r.ProviderMessage = pay.StatusFailure, mapCode(code), code, "declined by the bank"
	case !st.approved():
		r.Status, r.Code, r.ProviderMessage = pay.StatusFailure, mapCode(st.ProcReturnCode), st.ErrMsg
	case capturedSale(st):
		r.Status = pay.StatusSuccess
		if c, ok := captureAmount(st, a.Currency); ok && c >= 0 {
			r.PaidAmount = c
		}
	default:
		r.Status = pay.StatusPending
	}
	return r, nil
}

// Refund voids a full refund (same-day cancel, before the bank's end of day) and falls back to Credit
// when the bank declines the Void (batch already closed); a partial refund is a Credit. better-payment
// offers Void (cancel) and Credit (refund) separately; the fallback is this package's.
func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	ref := r.Attempt.Ref
	if r.Full {
		res, err := p.api(ctx, []kv{{"Type", "Void"}, {"OrderId", ref}})
		if err != nil {
			return nil, refundErr(err)
		}
		if res.approved() {
			return &pay.Result{Status: pay.StatusSuccess, TxnID: res.TransID, PaidAmount: r.Amount,
				Currency: r.Attempt.Currency, ProviderCode: res.ProcReturnCode, ProviderMessage: "void"}, nil
		}
	}
	cur := pay.NumericCurrency(r.Attempt.Currency)
	if cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported", r.Attempt.Currency)
	}
	res, err := p.api(ctx, []kv{
		{"Type", "Credit"},
		{"OrderId", ref},
		{"Total", pay.FormatMinor(r.Amount, r.Attempt.Currency)},
		{"Currency", cur},
	})
	if err != nil {
		return nil, refundErr(err)
	}
	if !res.approved() {
		return &pay.Result{Status: pay.StatusFailure, Code: mapCode(res.ProcReturnCode),
			ProviderCode: res.ProcReturnCode, ProviderMessage: res.ErrMsg}, nil
	}
	return &pay.Result{Status: pay.StatusSuccess, TxnID: res.TransID, PaidAmount: r.Amount,
		Currency: r.Attempt.Currency, ProviderCode: res.ProcReturnCode, ProviderMessage: "credit"}, nil
}

// refundErr turns any failure after the request may have reached the bank (unreadable answer, HTTP
// error) into ErrUnknownOutcome; a refund is never retried blindly.
func refundErr(err error) error {
	if errors.Is(err, pay.ErrUnknownOutcome) || errors.Is(err, errNotSent) {
		return err
	}
	return fmt.Errorf("%w: %w", pay.ErrUnknownOutcome, err)
}

var errNotSent = errors.New("pay: EST request not sent")

func (p *provider) orderStatus(ctx context.Context, oid string) (*response, error) {
	return p.apiExtra(ctx, []kv{{"OrderId", oid}, {"Mode", "P"}}, []kv{{"ORDERSTATUS", "SOR"}})
}

// api sends a CC5Request with the API user's credentials and parses the CC5Response.
func (p *provider) api(ctx context.Context, fields []kv) (*response, error) {
	return p.apiExtra(ctx, fields, nil)
}

func (p *provider) apiExtra(ctx context.Context, fields, extra []kv) (*response, error) {
	body := buildXML(append([]kv{{"Name", p.user}, {"Password", p.password}, {"ClientId", p.clientID}}, fields...), extra)
	status, out, err := pay.PostBody(ctx, p.cfg.HTTP, p.origin+apiPath, "text/xml; charset=ISO-8859-9", body, nil)
	if err != nil {
		if errors.Is(err, pay.ErrUnknownOutcome) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errNotSent, err)
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("pay: EST API answered HTTP %d", status)
	}
	return parseResponse(out)
}

func authMessage(authCode, hostRef string) string {
	var parts []string
	if authCode != "" {
		parts = append(parts, "AuthCode "+authCode)
	}
	if hostRef != "" {
		parts = append(parts, "HostRefNum "+hostRef)
	}
	return strings.Join(parts, ", ")
}

func installment(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// captureAmount reads CAPTURE_AMT of an order status answer in minor units. EST gives minor units,
// but some banks answer a decimal ("100.50"): that is parsed in the currency's decimals instead of
// skipping the comparison. A value that cannot be read reports -1 with ok true, so a comparison
// with it fails closed rather than being skipped.
func captureAmount(r *response, currency string) (int64, bool) {
	s := strings.TrimSpace(r.Extra["CAPTURE_AMT"])
	if s == "" {
		return 0, false
	}
	if strings.ContainsAny(s, ".,") {
		if n, err := pay.ParseMinor(s, currency); err == nil {
			return n, true
		}
		return -1, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return -1, true
	}
	return n, true
}

// capturedSale: an approved answer with TRANS_STAT S, C or A, charge type S and a captured amount.
func capturedSale(r *response) bool {
	ts := r.Extra["TRANS_STAT"]
	n, _ := strconv.ParseFloat(r.Extra["CAPTURE_AMT"], 64)
	return r.approved() && (ts == "S" || ts == "C" || ts == "A") && r.Extra["CHARGE_TYPE_CD"] == "S" && n > 0
}

// mapCode maps an ISO 8583 response code (better-payment ISO8583_ERROR_CODES) to a pay.Code.
func mapCode(c string) pay.Code {
	switch strings.TrimSpace(c) {
	case "01", "02", "05", "12", "57", "62":
		return pay.CodeCardDeclined
	case "03", "58":
		return pay.CodeConfiguration
	case "04", "07", "59":
		return pay.CodeFraudSuspected
	case "41", "43":
		return pay.CodeStolenOrLost
	case "13":
		return pay.CodeInvalidRequest
	case "14", "15", "82":
		return pay.CodeInvalidCard
	case "33", "54":
		return pay.CodeExpiredCard
	case "51":
		return pay.CodeInsufficientFunds
	case "61", "65":
		return pay.CodeLimitExceeded
	case "91", "96":
		return pay.CodeProviderDown
	case "94":
		return pay.CodeDuplicate
	}
	return pay.CodeUnknown
}

func random() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func firstValues(v url.Values) map[string]string {
	m := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m
}

// unhashed are the fields hash ver3 never covers (compared case-insensitively).
var unhashed = map[string]bool{"hash": true, "encoding": true, "countdown": true, "nationalidno": true}

// Hash is EST hash version 3: Base64(SHA-512(the values sorted by field name in case-insensitive
// natural order, then the store key, each with "\" escaped as "\\" and "|" as "\|", joined with
// "|")). Fields named hash, encoding, countdown and nationalidno are left out.
func Hash(fields [][2]string, storeKey string) string {
	var keep [][2]string
	for _, f := range fields {
		if !unhashed[strings.ToLower(f[0])] {
			keep = append(keep, f)
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return compareKeys(keep[i][0], keep[j][0]) < 0 })
	vals := make([]string, 0, len(keep)+1)
	for _, f := range keep {
		vals = append(vals, escape(f[1]))
	}
	vals = append(vals, escape(storeKey))
	sum := sha512.Sum512([]byte(strings.Join(vals, "|")))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Verify checks the HASH of the bank's callback over all other returned fields.
func Verify(fields map[string]string, storeKey string) bool {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([][2]string, 0, len(keys))
	for _, k := range keys {
		list = append(list, [2]string{k, fields[k]})
	}
	return pay.Equal(Hash(list, storeKey), fields["HASH"])
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `|`, `\|`).Replace(s)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// compareKeys is a case-insensitive natural order: digit runs compare by value ("a2" < "a10").
func compareKeys(a, b string) int {
	x, y := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(x) && j < len(y) {
		if isDigit(x[i]) && isDigit(y[j]) {
			ei, ej := i, j
			for ei < len(x) && isDigit(x[ei]) {
				ei++
			}
			for ej < len(y) && isDigit(y[ej]) {
				ej++
			}
			nx, ny := strings.TrimLeft(x[i:ei], "0"), strings.TrimLeft(y[j:ej], "0")
			if len(nx) != len(ny) {
				return len(nx) - len(ny)
			}
			if c := strings.Compare(nx, ny); c != 0 {
				return c
			}
			i, j = ei, ej
			continue
		}
		if x[i] != y[j] {
			if x[i] < y[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	return (len(x) - i) - (len(y) - j)
}
