//go:build !(wasip1 && wasm)

package wpgo

// invokeHost is the non-Wasm stub: there is no host to talk to, so every host
// function fails with ErrNotInHost. Hook registration and Dispatch still work,
// which lets plugin authors unit test callbacks with a normal toolchain.
func invokeHost(name string, body []byte) ([]byte, error) {
	switch name {
	case "add_hook", "log", "register_shortcode", "register_rest_route", "enqueue_style", "enqueue_script":
		// Registration and logging are best-effort outside the host.
		return []byte(`{"ok":true}`), nil
	case "register_settings_page":
		return []byte(`{"ok":true,"result":{"option_prefix":"plugin_test_"}}`), nil
	}
	return nil, ErrNotInHost
}
