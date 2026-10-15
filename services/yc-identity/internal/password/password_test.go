package password

import (
	"strings"
	"testing"
)

var fast = Params{Time: 1, MemoryKB: 8 * 1024, Threads: 1}

func TestHashAndVerify(t *testing.T) {
	h, err := NewHasher(fast)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("unexpected encoding %q", enc)
	}
	if ok, err := h.Verify("correct horse battery", enc); err != nil || !ok {
		t.Errorf("Verify(correct) = %v, %v", ok, err)
	}
	if ok, _ := h.Verify("wrong horse battery", enc); ok {
		t.Error("Verify(wrong) succeeded")
	}
	other, _ := h.Hash("correct horse battery")
	if other == enc {
		t.Error("hashes of the same password share a salt")
	}
}

func TestVerifyUsesStoredParams(t *testing.T) {
	old, _ := NewHasher(fast)
	enc, _ := old.Hash("pw-1234567890")
	stronger, _ := NewHasher(Params{Time: 2, MemoryKB: 16 * 1024, Threads: 2})
	if ok, err := stronger.Verify("pw-1234567890", enc); err != nil || !ok {
		t.Errorf("Verify with other params = %v, %v", ok, err)
	}
	if !stronger.NeedsRehash(enc) {
		t.Error("NeedsRehash = false for old params")
	}
	if old.NeedsRehash(enc) {
		t.Error("NeedsRehash = true for current params")
	}
}

func TestMalformed(t *testing.T) {
	h, _ := NewHasher(fast)
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$a$b", "$argon2id$v=18$m=8192,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=8192,t=1,p=1$!!$YQ"} {
		if _, err := h.Verify("x", bad); err == nil {
			t.Errorf("Verify(%q) succeeded", bad)
		}
	}
}

func TestWeakParamsRejected(t *testing.T) {
	if _, err := NewHasher(Params{Time: 1, MemoryKB: 1024, Threads: 1}); err == nil {
		t.Error("NewHasher accepted 1 MiB")
	}
}
