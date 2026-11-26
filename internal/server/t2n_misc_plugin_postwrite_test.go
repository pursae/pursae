package server

import (
	"context"
	"testing"

	"wpgo/internal/plugins"
	"wpgo/internal/store"
)

// Outside a request and outside a scheduled event a plugin has no user: it may create a draft but
// not rewrite an existing post. Inside a request it acts as the signed-in user.
func TestT2NMiscPluginPostWriteNeedsAUser(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	h := &pluginHost{s: srv}
	p := &store.Post{Type: "post", Status: "publish", Title: "Owned", Content: "<p>original</p>", AuthorID: 1}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	content := "<p>rewritten</p>"
	in := plugins.PostInput{Content: &content}

	if _, err := h.UpdatePost(ctx, "demo", p.ID, in); err == nil {
		t.Error("a plugin rewrote an existing post with no user and no scheduled event")
	}
	if got, _ := srv.store.GetPost(ctx, p.ID); got.Content != "<p>original</p>" {
		t.Errorf("content changed: %q", got.Content)
	}
	if _, err := h.UpdatePost(withPluginRequest(ctx, nil), "demo", p.ID, in); err == nil {
		t.Error("an anonymous request let a plugin rewrite a post")
	}
	title := "Draft from init"
	if _, err := h.InsertPost(ctx, "demo", plugins.PostInput{Title: &title}); err != nil {
		t.Errorf("a draft from a background caller was refused: %v", err)
	}
	admin, err := srv.store.UserByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.UpdatePost(withPluginRequest(ctx, admin), "demo", p.ID, in); err != nil {
		t.Errorf("the signed-in administrator's request was refused: %v", err)
	}
	if _, err := h.UpdatePost(withPluginElevated(ctx), "demo", p.ID, in); err != nil {
		t.Errorf("a scheduled event was refused: %v", err)
	}
	// Admin pages, widgets and meta boxes run with pluginUICtx: the request's user stays attached.
	uictx, cancel := pluginUICtx(withPluginRequest(ctx, admin))
	defer cancel()
	if pluginScope(uictx) != pluginScopeRequest || pluginUserFrom(uictx) != admin {
		t.Error("pluginUICtx dropped the request's user")
	}
}
