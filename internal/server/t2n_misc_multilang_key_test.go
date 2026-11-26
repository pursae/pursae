package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// The options API keeps the saved machine translation key only while it goes to the same provider
// and endpoint host; moving it elsewhere needs the key sent again.
func TestT2NMiscMultilangKeyFollowsEndpoint(t *testing.T) {
	_, _, srv := testServerBuilderOff(t)
	ctx := context.Background()
	o := srv.pllOpts()
	o.MTProvider, o.MTEndpoint, o.MTKey = "openai", "https://api.openai.com/v1", srv.pllSealMTKey("sk-original")
	if err := srv.pllSaveOpts(ctx, o); err != nil {
		t.Fatal(err)
	}
	save := func(patch string) error {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		return optionGroups["multilang"].save(srv, c, []byte(patch))
	}
	if err := save(`{"mt_endpoint": "https://collector.example.net/v1"}`); err == nil {
		t.Error("the endpoint host changed while the saved key was kept")
	}
	if got := srv.pllOpts(); got.MTEndpoint != "https://api.openai.com/v1" || srv.pllRevealMTKey(got.MTKey) != "sk-original" {
		t.Errorf("a refused update changed the settings: %+v", got)
	}
	if err := save(`{"mt_provider": "deepl"}`); err == nil {
		t.Error("the provider changed while the saved key was kept")
	}
	if err := save(`{"mt_endpoint": "https://api.openai.com/v1/"}`); err != nil {
		t.Errorf("same host refused: %v", err)
	}
	if err := save(`{"mt_endpoint": "https://other.example.net/v1", "mt_key": "sk-new"}`); err != nil {
		t.Fatalf("a new key with the new endpoint was refused: %v", err)
	}
	if got := srv.pllOpts(); got.MTEndpoint != "https://other.example.net/v1" || srv.pllRevealMTKey(got.MTKey) != "sk-new" {
		t.Errorf("after the update: %+v", got)
	}
}
