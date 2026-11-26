---
title: Payment provider sandbox tests
description: Run each hosted payment provider of Pursae Commerce against its real test endpoint with your own test merchant credentials, and when a provider may lose its Beta badge.
weight: 880
section: built-in-plugins
---

The provider packages under `internal/pay/` (PayTR, iyzico, Sipay, the EST banks, Akbank, Garanti BBVA,
QNB, VakıfBank, PayPal) have unit tests against fake servers, but a fake server only proves that the code
matches our reading of the documentation. The sandbox tests run the same code against the provider's
**real test endpoint** with a test merchant account. They are not part of `go test ./...`: every
`sandbox_test.go` has the build tag `sandbox`, so normal builds do not compile them, and without
credentials they skip themselves.

## What they do

| Test | What it checks | Needs |
|---|---|---|
| `TestSandboxStart` | `Start` with a fresh random reference and 10.00 TRY (PayPal: 10.00 USD) returns exactly one of redirect, form or embed, on an https URL on the provider's hosts; no configured secret appears in what goes to the browser. It logs where to open the page (for a form, set `PAY_SANDBOX_OUT_DIR` to get a self-submitting HTML file) so you can pay with the provider's test card. | the provider's credentials |
| `TestSandboxQueryUnknown` | `Query` for a reference that was never paid is pending, failure or not found, never success (or `ErrNotSupported`). A configuration error fails the test. | the provider's credentials |
| `TestSandboxComplete` | `Complete` on a callback you captured after a test payment is a success with the paid amount; the same callback with one character changed is not. | credentials and the captured callback |
| `TestSandboxRefund` | Refunds 1.00 of a test payment and logs the result. | credentials and a paid test payment |

The tests build the provider with `Test: true` and never set `BaseURL`, so they always talk to the
provider's own test host. Credentials are read only from environment variables; nothing is stored in the
repository. The card is typed on the provider's page by a person, never by the tests.

## Variables

All names are `PAY_SANDBOX_<PROVIDER>_<FIELD>`; `*` marks a field the test needs (a test whose required
variables are missing is skipped and names them). `PROVIDER` is the provider's id in upper case; each EST
bank has its own set, for example `PAY_SANDBOX_EST_ISBANK_CLIENT_ID`. The fields are the provider's
settings in Settings > Payments.

| Provider | Credentials and options | Test currency |
|---|---|---|
| PayTR (`paytr`) | `PAY_SANDBOX_PAYTR_MERCHANT_ID`*<br>`PAY_SANDBOX_PAYTR_MERCHANT_KEY`*<br>`PAY_SANDBOX_PAYTR_MERCHANT_SALT`*<br>`PAY_SANDBOX_PAYTR_DARK` | TRY |
| iyzico (`iyzico`) | `PAY_SANDBOX_IYZICO_API_KEY`*<br>`PAY_SANDBOX_IYZICO_SECRET_KEY`* | TRY |
| Sipay (`sipay`) | `PAY_SANDBOX_SIPAY_MERCHANT_KEY`*<br>`PAY_SANDBOX_SIPAY_APP_ID`*<br>`PAY_SANDBOX_SIPAY_APP_SECRET`* | TRY |
| İş Bankası (`est_isbank`) | `PAY_SANDBOX_EST_ISBANK_CLIENT_ID`*<br>`PAY_SANDBOX_EST_ISBANK_STORE_KEY`*<br>`PAY_SANDBOX_EST_ISBANK_API_USER`*<br>`PAY_SANDBOX_EST_ISBANK_API_PASSWORD`*<br>`PAY_SANDBOX_EST_ISBANK_STORE_TYPE` | TRY |
| Ziraat Bankası (`est_ziraat`) | `PAY_SANDBOX_EST_ZIRAAT_CLIENT_ID`*<br>`PAY_SANDBOX_EST_ZIRAAT_STORE_KEY`*<br>`PAY_SANDBOX_EST_ZIRAAT_API_USER`*<br>`PAY_SANDBOX_EST_ZIRAAT_API_PASSWORD`*<br>`PAY_SANDBOX_EST_ZIRAAT_STORE_TYPE` | TRY |
| Halkbank (`est_halkbank`) | `PAY_SANDBOX_EST_HALKBANK_CLIENT_ID`*<br>`PAY_SANDBOX_EST_HALKBANK_STORE_KEY`*<br>`PAY_SANDBOX_EST_HALKBANK_API_USER`*<br>`PAY_SANDBOX_EST_HALKBANK_API_PASSWORD`*<br>`PAY_SANDBOX_EST_HALKBANK_STORE_TYPE` | TRY |
| TEB (`est_teb`) | `PAY_SANDBOX_EST_TEB_CLIENT_ID`*<br>`PAY_SANDBOX_EST_TEB_STORE_KEY`*<br>`PAY_SANDBOX_EST_TEB_API_USER`*<br>`PAY_SANDBOX_EST_TEB_API_PASSWORD`*<br>`PAY_SANDBOX_EST_TEB_STORE_TYPE` | TRY |
| Şekerbank (`est_sekerbank`) | `PAY_SANDBOX_EST_SEKERBANK_CLIENT_ID`*<br>`PAY_SANDBOX_EST_SEKERBANK_STORE_KEY`*<br>`PAY_SANDBOX_EST_SEKERBANK_API_USER`*<br>`PAY_SANDBOX_EST_SEKERBANK_API_PASSWORD`*<br>`PAY_SANDBOX_EST_SEKERBANK_STORE_TYPE` | TRY |
| Akbank, legacy EST POS (`est_akbank`) | `PAY_SANDBOX_EST_AKBANK_CLIENT_ID`*<br>`PAY_SANDBOX_EST_AKBANK_STORE_KEY`*<br>`PAY_SANDBOX_EST_AKBANK_API_USER`*<br>`PAY_SANDBOX_EST_AKBANK_API_PASSWORD`*<br>`PAY_SANDBOX_EST_AKBANK_STORE_TYPE` | TRY |
| Akbank (`akbank`) | `PAY_SANDBOX_AKBANK_MERCHANT_SAFE_ID`*<br>`PAY_SANDBOX_AKBANK_TERMINAL_SAFE_ID`*<br>`PAY_SANDBOX_AKBANK_SECRET_KEY`* | TRY |
| GarantiPay, Garanti BBVA (`garanti`) | `PAY_SANDBOX_GARANTI_MERCHANT_ID`*<br>`PAY_SANDBOX_GARANTI_TERMINAL_ID`*<br>`PAY_SANDBOX_GARANTI_PROVISION_PASSWORD`*<br>`PAY_SANDBOX_GARANTI_STORE_KEY`*<br>`PAY_SANDBOX_GARANTI_REFUND_PASSWORD`<br>`PAY_SANDBOX_GARANTI_COMPANY_NAME` | TRY |
| QNB (`qnb`) | `PAY_SANDBOX_QNB_MERCHANT_ID`*<br>`PAY_SANDBOX_QNB_USER_CODE`*<br>`PAY_SANDBOX_QNB_USER_PASS`*<br>`PAY_SANDBOX_QNB_MERCHANT_PASS`* | TRY |
| VakıfBank (`vakifbank`) | `PAY_SANDBOX_VAKIFBANK_MERCHANT_ID`*<br>`PAY_SANDBOX_VAKIFBANK_PASSWORD`*<br>`PAY_SANDBOX_VAKIFBANK_TERMINAL_NO`* | TRY |
| PayPal (`paypal`) | `PAY_SANDBOX_PAYPAL_CLIENT_ID`*<br>`PAY_SANDBOX_PAYPAL_CLIENT_SECRET`*<br>`PAY_SANDBOX_PAYPAL_WEBHOOK_ID` | USD |

The table lists the Spec fields as they are today; when a provider gains a field, its variable follows the
same rule. Shared variables, all optional:

| Variable | Meaning |
|---|---|
| `PAY_SANDBOX_RETURN_BASE` | Origin for the return, fail and notify URLs of the test payment. Default `https://example.test`. Use a public https address of a store (or a tunnel) when the provider must be able to reach it. |
| `PAY_SANDBOX_CLIENT_IP` | The customer IP sent to providers that want a real, public address. Default is a documentation address (`203.0.113.10`). |
| `PAY_SANDBOX_OUT_DIR` | A folder where `TestSandboxStart` writes `<provider>-start.html` for providers that start with a form post. |

## Where to get test credentials

Test credentials are issued by the provider to a merchant; Pursae cannot create them. Start from the
provider's developer pages:

| Provider | Start here |
|---|---|
| PayTR | https://dev.paytr.com |
| iyzico | https://docs.iyzico.com |
| Sipay | https://apidocs.sipay.com.tr |
| EST banks (İş Bankası, Ziraat, Halkbank, TEB, Şekerbank, legacy Akbank) | https://entegrasyon.asseco-see.com.tr (the test POS of your bank comes from the bank's e-commerce team) |
| Akbank | https://apiportal.akbank.com |
| Garanti BBVA | https://dev.garantibbva.com.tr |
| QNB | no public self-service portal is known; ask QNB for a test virtual POS |
| VakıfBank | https://sanalpos.innova.com.tr/vakifbankasi/ (the test POS comes from the bank) |
| PayPal | https://developer.paypal.com (sandbox app: client id and secret) |

## Running one provider

```sh
export PAY_SANDBOX_PAYTR_MERCHANT_ID=...      # use a shell that does not keep history, or a .env you do not commit
export PAY_SANDBOX_PAYTR_MERCHANT_KEY=...
export PAY_SANDBOX_PAYTR_MERCHANT_SALT=...
go test -tags sandbox ./internal/pay/paytr/ -run Sandbox -v
```

With no variables set, `go vet -tags sandbox ./internal/pay/...` and
`go test -tags sandbox ./internal/pay/... -count=1` pass and every test skips.

## Capturing a callback for `TestSandboxComplete`

1. Run `TestSandboxStart -v`. It logs the reference (`ref ...`) and the provider reference.
   Use the same `PAY_SANDBOX_RETURN_BASE`, and make it an address you can read requests on (a tunnel to a
   small logging server, or a request bin).
2. Open the logged page and pay with the provider's test card (the provider documents them).
3. Save the raw body of the request the provider sends to the return or notify URL into a file: a form post
   as its urlencoded body (`a=1&b=2`), a JSON notification as is. Do not reformat or trim it.
4. Run:

```sh
export PAY_SANDBOX_PAYTR_CALLBACK=/path/to/body.txt
export PAY_SANDBOX_PAYTR_REF=<the ref from step 1>
export PAY_SANDBOX_PAYTR_AMOUNT=1000                 # minor units: 10.00 is 1000
export PAY_SANDBOX_PAYTR_PROVIDER_REF=<provider ref from step 1, if it logged one>
go test -tags sandbox ./internal/pay/paytr/ -run SandboxComplete -v
```

A form body is sent as a return callback and a JSON body as a notification; set
`PAY_SANDBOX_<PROVIDER>_CALLBACK_KIND` to `return`, `fail` or `notify` when the provider posted it to
another URL. The test also flips one character of the body and expects that to fail.

## Refund

```sh
export PAY_SANDBOX_PAYTR_REFUND_REF=<the ref of a paid test payment>
export PAY_SANDBOX_PAYTR_REFUND_TXN=<its transaction id>
export PAY_SANDBOX_PAYTR_REFUND_AMOUNT=1000          # what that payment charged, in minor units
# optional: PAY_SANDBOX_PAYTR_REFUND_PROVIDER_REF
go test -tags sandbox ./internal/pay/paytr/ -run SandboxRefund -v
```

It refunds 1.00 (the request is a partial refund unless the payment was exactly 1.00) and logs status,
transaction id and the provider's code. Some providers allow a refund only after the day's settlement and
a void only before it; a failure is worth reading before it is treated as a bug.

## When a provider may lose its Beta badge

A provider's `Verified` flag, or its Beta badge in Settings > Payments, is removed only after all four steps
have passed against the provider's real test environment, on the version being released:

1. `Start`, and the page it returns opens;
2. paying with a test card on that page;
3. the callback (return and notification as the provider uses them) completes the order with the right
   amount (`TestSandboxComplete` with a callback captured from that payment);
4. a refund (`TestSandboxRefund`).

Passing the unit tests alone is not enough.
