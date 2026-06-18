package leaf

import "testing"

const testHex64 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// TestValidateContainerImageImmutable verifies the publish-time immutability lint
// (TODO #38): bare or :latest container refs are rejected; digest-pinned or explicit
// non-latest tags are accepted. Registry-port colons must not be mistaken for tags.
func TestValidateContainerImageImmutable(t *testing.T) {
	reject := []string{
		"",
		"registry.example.com/foo",         // bare -> defaults to :latest
		"registry.example.com/foo:latest",  // explicit mutable
		"foo:latest",
		"foo",
		"domain/img:LATEST", // case-insensitive
		"registry:5000/foo", // port colon, no tag -> bare
	}
	for _, img := range reject {
		if validateContainerImageImmutable(img) == nil {
			t.Errorf("expected %q to be REJECTED", img)
		}
	}

	accept := []string{
		"registry.example.com/foo:2.0",
		"foo:v1.2.3",
		"domain/img:abcdef",
		"registry.example.com:5000/foo:2.0", // port colon + real tag
		"domain/img@sha256:" + testHex64,
		"lbry.science/beyblade@sha256:" + testHex64,
	}
	for _, img := range accept {
		if err := validateContainerImageImmutable(img); err != nil {
			t.Errorf("expected %q to be ACCEPTED, got %v", img, err)
		}
	}
}

func TestImageDigestFromRef(t *testing.T) {
	digest := "sha256:" + testHex64
	if got := imageDigestFromRef("repo/img@" + digest); got != digest {
		t.Errorf("digest ref: want %q, got %q", digest, got)
	}
	if got := imageDigestFromRef("repo/img:2.0"); got != "" {
		t.Errorf("tag ref: want empty, got %q", got)
	}
	if got := imageDigestFromRef(""); got != "" {
		t.Errorf("empty ref: want empty, got %q", got)
	}
}
