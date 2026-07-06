package redisstate

import "testing"

func TestStoreKeyNormalizesPrefixAndParts(t *testing.T) {
	t.Parallel()

	store := &Store{keyPrefix: " :team: "}
	got := store.key(" cache ", ":usage:", "", " :daily: ")
	if got != "team:cache:usage:daily" {
		t.Fatalf("key() = %q, want %q", got, "team:cache:usage:daily")
	}
}

func TestStoreKeyUsesDefaultPrefix(t *testing.T) {
	t.Parallel()

	store := &Store{}
	got := store.key("runtime", "auth")
	if got != "cliproxyapi:runtime:auth" {
		t.Fatalf("key() = %q, want %q", got, "cliproxyapi:runtime:auth")
	}
}

func BenchmarkStoreKey(b *testing.B) {
	store := &Store{keyPrefix: "cliproxyapi"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = store.key("proxy-pool", "leases", "auth-1234567890")
	}
}
