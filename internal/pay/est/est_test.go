package est

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"wpgo/internal/pay"
)

const storeKey = "TESTSTOREKEY"

func cfgValues(storeType string) map[string]string {
	return map[string]string{
		"client_id": "100100000", "store_key": storeKey,
		"api_user": "apiuser", "api_password": "apipass", "store_type": storeType,
	}
}

func newP(t *testing.T, storeType, base string, test bool) *provider {
	t.Helper()
	p, err := newProvider(banks[0], pay.Config{Values: cfgValues(storeType), Test: test,
		HTTP: &http.Client{Timeout: 2 * time.Second}, BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Known vectors from better-payment tests/fixtures/est.ts (computed there with Python hashlib).
func TestHashReferenceVectors(t *testing.T) {
	pay3d := map[string]string{
		"clientid": "100100000", "oid": "ORDERPAY", "amount": "1.20", "currency": "949", "taksit": "",
		"storetype": "3d_pay", "mdStatus": "1", "md": "MDVALUE123", "xid": "XIDVALUE=", "eci": "02",
		"cavv": "CAVVVALUE=", "mdErrorMsg": "Authenticated", "hashAlgorithm": "ver3",
		"rnd": "0123456789abcdef0123456789abcdef", "Response": "Approved", "ProcReturnCode": "00",
		"AuthCode": "123456", "TransId": "26011AAAA1234", "ErrMsg": "",
		"HASH": "3udQQXgx3JMKdHwJsnTtHgV1X54DLRRf8KseAe34pLatCK6zsxZD6vnqNfZzDDnAFfruS1/G1qIdGImh1Jp2zQ==",
	}
	if !Verify(pay3d, storeKey) {
		t.Fatal("3d_pay fixture does not verify")
	}
	d3 := map[string]string{
		"clientid": "100100000", "oid": "ORDER3D", "amount": "1.20", "currency": "949", "taksit": "",
		"storetype": "3d", "mdStatus": "1", "md": "MDVALUE123", "xid": "XIDVALUE=", "eci": "02",
		"cavv": "CAVVVALUE=", "mdErrorMsg": "Authenticated", "hashAlgorithm": "ver3",
		"rnd": "0123456789abcdef0123456789abcdef", "Response": "", "ProcReturnCode": "",
		"encoding": "ISO-8859-9",
		"HASH":     "/DO0HeanIuQx/ETIXeB4y6o/3bLMZl4qnQeyQpGnS9mj3tlyHsZ3PP8KE06IsYaEy3PGayXNjVnSusKsTUlaFg==",
	}
	if !Verify(d3, storeKey) {
		t.Fatal("3d fixture does not verify (encoding must be excluded)")
	}
	if Verify(pay3d, "OTHERKEY") {
		t.Fatal("verified with a wrong key")
	}
}

func sha(s string) string {
	h := sha512.Sum512([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

func TestHashIndependent(t *testing.T) {
	fields := [][2]string{
		{"b10", `x|y`}, {"B2", `a\b`}, {"a", "1"}, {"hash", "ignored"}, {"countdown", "9"},
		{"NationalIdNo", "1"}, {"Encoding", "utf-8"},
	}
	// Sorted case-insensitively, naturally: a, B2, b10.
	want := sha(`1|a\\b|x\|y|k\|e\\y`)
	if got := Hash(fields, `k|e\y`); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if compareKeys("a2", "a10") >= 0 || compareKeys("Email", "amount") <= 0 || compareKeys("oid", "okUrl") >= 0 {
		t.Fatal("bad key order")
	}
}

func formMap(f *pay.Form) map[string]string {
	m := map[string]string{}
	for _, kv := range f.Fields {
		m[kv[0]] = kv[1]
	}
	return m
}

func startReq(inst int) pay.StartRequest {
	return pay.StartRequest{Ref: "REF12345678", Amount: 34990, Currency: "TRY", Installment: inst,
		ReturnURL: "https://shop.test/ok", FailURL: "https://shop.test/fail", Locale: "en",
		Customer: pay.Customer{Email: "buyer@example.com"}}
}

func TestStart(t *testing.T) {
	p := newP(t, storePayHosting, "", false)
	res, err := p.Start(context.Background(), startReq(1))
	if err != nil {
		t.Fatal(err)
	}
	if res.Form.Action != "https://sanalpos.isbank.com.tr/fim/est3Dgate" {
		t.Fatal(res.Form.Action)
	}
	var names []string
	for _, f := range res.Form.Fields {
		names = append(names, f[0])
	}
	want := "clientid,storetype,amount,oid,okUrl,failUrl,TranType,currency,rnd,lang,hashAlgorithm,refreshTime,Email,hash"
	if strings.Join(names, ",") != want {
		t.Fatalf("fields %v", names)
	}
	m := formMap(res.Form)
	for _, bad := range []string{"pan", "cv2", "cardType", "cardHolderName"} {
		if _, ok := m[bad]; ok {
			t.Fatalf("card field %s present", bad)
		}
	}
	for k := range m {
		if strings.HasPrefix(k, "Ecom_Payment_Card") {
			t.Fatalf("card field %s", k)
		}
	}
	if m["amount"] != "349.90" || m["currency"] != "949" || m["oid"] != "REF12345678" || m["storetype"] != "3d_pay_hosting" ||
		m["lang"] != "en" || m["hashAlgorithm"] != "ver3" || m["okUrl"] != "https://shop.test/ok" || m["failUrl"] != "https://shop.test/fail" {
		t.Fatalf("fields %v", m)
	}
	// The bank's side: verify over every posted field.
	posted := map[string]string{}
	for k, v := range m {
		posted[k] = v
	}
	posted["HASH"] = posted["hash"]
	delete(posted, "hash")
	if !Verify(posted, storeKey) {
		t.Fatal("start hash does not verify")
	}

	res, _ = p.Start(context.Background(), startReq(6))
	if formMap(res.Form)["taksit"] != "6" {
		t.Fatal("installment missing")
	}
}

func TestEndpoints(t *testing.T) {
	specs := Specs()
	ids := map[string]bool{}
	for _, s := range specs {
		ids[s.ID] = true
		if s.Region != pay.RegionTR || s.Kind != pay.KindBank || len(s.Hosts.Form) != 2 || s.Hosts.Form[1] != testOrigin {
			t.Fatalf("spec %+v", s.ID)
		}
		pr, err := s.New(pay.Config{Values: cfgValues(""), HTTP: http.DefaultClient})
		if err != nil {
			t.Fatal(err)
		}
		res, _ := pr.Start(context.Background(), startReq(0))
		if !strings.HasPrefix(res.Form.Action, s.Hosts.Form[0]) {
			t.Fatalf("%s live action %s", s.ID, res.Form.Action)
		}
		pt, _ := s.New(pay.Config{Values: cfgValues(""), HTTP: http.DefaultClient, Test: true})
		res, _ = pt.Start(context.Background(), startReq(0))
		if res.Form.Action != testOrigin+gatePath {
			t.Fatalf("%s test action %s", s.ID, res.Form.Action)
		}
	}
	for _, id := range []string{"est_isbank", "est_ziraat", "est_halkbank", "est_teb", "est_sekerbank", "est_akbank"} {
		if !ids[id] {
			t.Fatal("missing", id)
		}
	}
	if _, err := specs[0].New(pay.Config{Values: map[string]string{"client_id": "x"}}); err == nil {
		t.Fatal("incomplete settings accepted")
	}
	if _, err := specs[0].New(pay.Config{Values: cfgValues("3d")}); err == nil {
		t.Fatal("3d store type accepted")
	}
}

// bank is a fake XML API: it records requests and answers from a queue.
type fakeBank struct {
	mu      sync.Mutex
	bodies  []string
	answers []string
	delay   time.Duration
}

func (b *fakeBank) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bodies = append(b.bodies, string(body))
	if r.URL.Path != apiPath || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/xml") {
		w.WriteHeader(404)
		return
	}
	a := b.answers[0]
	if len(b.answers) > 1 {
		b.answers = b.answers[1:]
	}
	io.WriteString(w, a)
}

func cc5(fields string, extra string) string {
	s := `<?xml version="1.0" encoding="ISO-8859-9"?><CC5Response>` + fields
	if extra != "" {
		s += "<Extra>" + extra + "</Extra>"
	}
	return s + "</CC5Response>"
}

const approved = `<OrderId>REF12345678</OrderId><Response>Approved</Response><AuthCode>123456</AuthCode><HostRefNum>601</HostRefNum><ProcReturnCode>00</ProcReturnCode><TransId>T1</TransId><ErrMsg></ErrMsg>`

func statusXML(transStat, charge, amt string) string {
	return cc5(`<ErrMsg>Record(s) found</ErrMsg><ProcReturnCode>00</ProcReturnCode><Response>Approved</Response><OrderId>REF12345678</OrderId><TransId>T1</TransId>`,
		"<TRANS_STAT>"+transStat+"</TRANS_STAT><CHARGE_TYPE_CD>"+charge+"</CHARGE_TYPE_CD><CAPTURE_AMT>"+amt+"</CAPTURE_AMT>")
}

func server(t *testing.T, b *fakeBank) string {
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	return s.URL
}

func signed(f map[string]string) url.Values {
	delete(f, "HASH")
	var list [][2]string
	for k, v := range f {
		list = append(list, [2]string{k, v})
	}
	// Map order is random; Hash sorts by name, the keys here are distinct case-insensitively.
	h := Hash(list, storeKey)
	v := url.Values{}
	for k, x := range f {
		v.Set(k, x)
	}
	v.Set("HASH", h)
	return v
}

func cb(over map[string]string) map[string]string {
	f := map[string]string{
		"clientid": "100100000", "oid": "REF12345678", "amount": "349.90", "currency": "949", "taksit": "",
		"storetype": "3d_pay_hosting", "mdStatus": "1", "hashAlgorithm": "ver3", "rnd": "abc",
		"Response": "Approved", "ProcReturnCode": "00", "AuthCode": "123456", "TransId": "T1", "ErrMsg": "",
	}
	for k, v := range over {
		f[k] = v
	}
	return f
}

var att = pay.Attempt{Ref: "REF12345678", Amount: 34990, Currency: "TRY"}

func complete(t *testing.T, p *provider, form url.Values) (*pay.Result, error) {
	return p.Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackReturn, Method: "POST", Form: form}, att)
}

func TestCompleteApproved(t *testing.T) {
	b := &fakeBank{answers: []string{statusXML("S", "S", "34990")}}
	p := newP(t, storePayHosting, server(t, b), true)
	form := signed(cb(nil))
	if p.CallbackRef(pay.CallbackInput{Form: form}) != "REF12345678" {
		t.Fatal("callback ref")
	}
	r, err := complete(t, p, form)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != pay.StatusSuccess || r.PaidAmount != 34990 || r.Currency != "TRY" || r.TxnID != "T1" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(b.bodies[0], "<Extra><ORDERSTATUS>SOR</ORDERSTATUS></Extra>") ||
		!strings.Contains(b.bodies[0], "<Name>apiuser</Name><Password>apipass</Password><ClientId>100100000</ClientId><OrderId>REF12345678</OrderId>") {
		t.Fatal(b.bodies[0])
	}
}

func TestCompleteRejections(t *testing.T) {
	b := &fakeBank{answers: []string{statusXML("S", "S", "34990")}}
	p := newP(t, storePayHosting, server(t, b), true)

	tampered := signed(cb(nil))
	tampered.Set("amount", "1.00")
	if _, err := complete(t, p, tampered); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatal("tampered amount accepted", err)
	}
	tampered = signed(cb(nil))
	tampered.Set("oid", "REFOTHER0000")
	if _, err := complete(t, p, tampered); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatal("tampered oid accepted", err)
	}
	missing := signed(cb(nil))
	missing.Del("HASH")
	if _, err := complete(t, p, missing); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatal("missing hash accepted", err)
	}
	if _, err := complete(t, p, signed(cb(map[string]string{"oid": "REFOTHER0000"}))); err == nil {
		t.Fatal("other order accepted")
	}
	if _, err := complete(t, p, signed(cb(map[string]string{"clientid": "999"}))); err == nil {
		t.Fatal("other store accepted")
	}
	if len(b.bodies) != 0 {
		t.Fatal("bank asked for a rejected callback")
	}

	r, err := complete(t, p, signed(cb(map[string]string{"mdStatus": "0"})))
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.Code3DSFailed {
		t.Fatalf("bad mdStatus: %+v %v", r, err)
	}
	r, err = complete(t, p, signed(cb(map[string]string{"Response": "Declined", "ProcReturnCode": "51", "ErrMsg": "Yetersiz bakiye"})))
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeInsufficientFunds || r.ProviderCode != "51" {
		t.Fatalf("declined: %+v %v", r, err)
	}
	for code, want := range map[string]pay.Code{"54": pay.CodeExpiredCard, "05": pay.CodeCardDeclined, "43": pay.CodeStolenOrLost, "57": pay.CodeCardDeclined, "99": pay.CodeUnknown} {
		if mapCode(code) != want {
			t.Fatal("map", code)
		}
	}
}

func TestCompleteBankDisagrees(t *testing.T) {
	b := &fakeBank{answers: []string{statusXML("S", "S", "100")}}
	p := newP(t, storePayHosting, server(t, b), true)
	if _, err := complete(t, p, signed(cb(nil))); err == nil {
		t.Fatal("amount mismatch with the bank accepted")
	}
	b.answers = []string{statusXML("V", "S", "34990")}
	if _, err := complete(t, p, signed(cb(nil))); err == nil {
		t.Fatal("voided order accepted")
	}
}

func TestComplete3DHost(t *testing.T) {
	b := &fakeBank{answers: []string{cc5(approved, "")}}
	p := newP(t, storeHost, server(t, b), true)
	f := cb(map[string]string{"storetype": "3d_host", "Response": "", "ProcReturnCode": "", "md": "MDTOKEN", "xid": "X=", "eci": "02", "cavv": "C="})
	r, err := complete(t, p, signed(f))
	if err != nil || r.Status != pay.StatusSuccess || r.TxnID != "T1" || r.PaidAmount != 34990 {
		t.Fatalf("%+v %v", r, err)
	}
	if !strings.Contains(b.bodies[0], "<Type>Auth</Type><OrderId>REF12345678</OrderId><Total>349.90</Total><Currency>949</Currency><Taksit></Taksit><Number>MDTOKEN</Number>") {
		t.Fatal(b.bodies[0])
	}
	// 3d_host charges only after full authentication.
	r, err = complete(t, p, signed(cb(map[string]string{"mdStatus": "2", "md": "MDTOKEN"})))
	if err != nil || r.Code != pay.Code3DSFailed {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestQuery(t *testing.T) {
	b := &fakeBank{answers: []string{statusXML("S", "S", "34990")}}
	p := newP(t, storePayHosting, server(t, b), true)
	r, err := p.Query(context.Background(), att)
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 34990 || r.TxnID != "T1" {
		t.Fatalf("%+v %v", r, err)
	}
	b.answers = []string{statusXML("V", "S", "34990")}
	if r, _ = p.Query(context.Background(), att); r.Status != pay.StatusCancelled {
		t.Fatal(r.Status)
	}
	b.answers = []string{cc5(`<ErrMsg>No record found</ErrMsg><ProcReturnCode>99</ProcReturnCode><Response>Error</Response>`, "")}
	if r, _ = p.Query(context.Background(), att); r.Status != pay.StatusFailure || r.Code != pay.CodeNotFound {
		t.Fatalf("%+v", r)
	}
}

func refundReq(full bool, amount int64) pay.RefundRequest {
	a := att
	a.PaidAmount = 34990
	return pay.RefundRequest{Attempt: a, Amount: amount, Full: full, RefundRef: "RF1"}
}

func TestRefundVoid(t *testing.T) {
	b := &fakeBank{answers: []string{cc5(approved, "")}}
	p := newP(t, storePayHosting, server(t, b), true)
	r, err := p.Refund(context.Background(), refundReq(true, 34990))
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 34990 {
		t.Fatalf("%+v %v", r, err)
	}
	want := `<?xml version="1.0" encoding="ISO-8859-9"?><CC5Request><Name>apiuser</Name><Password>apipass</Password><ClientId>100100000</ClientId><Type>Void</Type><OrderId>REF12345678</OrderId></CC5Request>`
	if len(b.bodies) != 1 || b.bodies[0] != want {
		t.Fatal(b.bodies)
	}
}

func TestRefundVoidThenCredit(t *testing.T) {
	b := &fakeBank{answers: []string{
		cc5(`<Response>Error</Response><ProcReturnCode>99</ProcReturnCode><ErrMsg>Void not possible</ErrMsg>`, ""),
		cc5(approved, ""),
	}}
	p := newP(t, storePayHosting, server(t, b), true)
	r, err := p.Refund(context.Background(), refundReq(true, 34990))
	if err != nil || r.Status != pay.StatusSuccess {
		t.Fatalf("%+v %v", r, err)
	}
	if len(b.bodies) != 2 || !strings.Contains(b.bodies[1], "<Type>Credit</Type><OrderId>REF12345678</OrderId><Total>349.90</Total><Currency>949</Currency>") {
		t.Fatal(b.bodies)
	}
}

func TestRefundPartial(t *testing.T) {
	b := &fakeBank{answers: []string{cc5(approved, "")}}
	p := newP(t, storePayHosting, server(t, b), true)
	r, err := p.Refund(context.Background(), refundReq(false, 1000))
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 1000 {
		t.Fatalf("%+v %v", r, err)
	}
	if len(b.bodies) != 1 || !strings.Contains(b.bodies[0], "<Type>Credit</Type><OrderId>REF12345678</OrderId><Total>10.00</Total>") {
		t.Fatal(b.bodies)
	}
	b.answers = []string{cc5(`<Response>Error</Response><ProcReturnCode>99</ProcReturnCode><ErrMsg>Iade tutari fazla</ErrMsg>`, "")}
	if r, err = p.Refund(context.Background(), refundReq(false, 1000)); err != nil || r.Status != pay.StatusFailure {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRefundTimeout(t *testing.T) {
	b := &fakeBank{answers: []string{cc5(approved, "")}, delay: 300 * time.Millisecond}
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	p, _ := newProvider(banks[0], pay.Config{Values: cfgValues(""), HTTP: &http.Client{Timeout: 50 * time.Millisecond}, BaseURL: s.URL})
	if _, err := p.Refund(context.Background(), refundReq(false, 1000)); !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatal(err)
	}
}

func TestXMLEscaping(t *testing.T) {
	got := string(buildXML([]kv{{"Name", "Ayşe & <Ç>"}}, nil))
	if !strings.Contains(got, "<Name>Ay&#351;e &amp; &lt;&#199;&gt;</Name>") {
		t.Fatal(got)
	}
	if _, err := parseResponse([]byte("not xml")); err == nil {
		t.Fatal("parsed garbage")
	}
}

// A decimal CAPTURE_AMT is compared in minor units, not skipped; an unreadable one fails closed.
func TestCaptureAmountDecimal(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{{"", 0, false}, {"10050", 10050, true}, {"100.50", 10050, true}, {"100,50", 10050, true}, {"1.005", -1, true}, {"abc", -1, true}} {
		got, ok := captureAmount(&response{Extra: map[string]string{"CAPTURE_AMT": c.in}}, "TRY")
		if got != c.want || ok != c.ok {
			t.Errorf("captureAmount(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
