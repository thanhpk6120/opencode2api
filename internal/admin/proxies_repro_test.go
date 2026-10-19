package admin

import "testing"

// Saving the masked address the UI displays would overwrite the real
// credentials with the literal "***" mask, so it must be rejected.
func TestRejectRedactedProxyValue(t *testing.T) {
	for _, masked := range []string{
		"http://***@103.69.188.53:40719",
		"http://%2A%2A%2A@103.69.188.53:40719",
		"socks5://***@10.0.0.1:1080",
	} {
		if err := rejectRedactedProxyValue(masked); err == nil {
			t.Fatalf("masked value accepted: %q", masked)
		}
	}
	for _, real := range []string{
		"http://sp07v6-40719:PVAFH@103.69.188.53:40719",
		"http://sp07v6-40719:PVAFH@103.69.188.53:40719#pool",
		"103.69.188.53:40719:user:pass",
	} {
		if err := rejectRedactedProxyValue(real); err != nil {
			t.Fatalf("real value rejected: %q: %v", real, err)
		}
	}
}
