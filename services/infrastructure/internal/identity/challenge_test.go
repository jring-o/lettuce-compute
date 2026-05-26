package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// mockChallengeStore implements ChallengeStore for testing.
type mockChallengeStore struct {
	challenges map[types.ID]*Challenge
}

func newMockStore() *mockChallengeStore {
	return &mockChallengeStore{challenges: make(map[types.ID]*Challenge)}
}

func (s *mockChallengeStore) Create(ctx context.Context, publicKey []byte) (*Challenge, error) {
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return nil, err
	}

	now := types.Now()
	c := &Challenge{
		ID:        types.NewID(),
		PublicKey: publicKey,
		Challenge: challengeBytes,
		ExpiresAt: now.Add(ChallengeExpiry),
		Verified:  false,
		CreatedAt: now,
	}
	s.challenges[c.ID] = c
	return c, nil
}

func (s *mockChallengeStore) Get(ctx context.Context, challengeID types.ID) (*Challenge, error) {
	c, ok := s.challenges[challengeID]
	if !ok {
		return nil, nil
	}
	return c, nil
}

func (s *mockChallengeStore) Verify(ctx context.Context, challengeID types.ID) error {
	if c, ok := s.challenges[challengeID]; ok {
		c.Verified = true
	}
	return nil
}

func TestMockChallengeStore_CreateAndGet(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	challenge, err := store.Create(ctx, pub)
	if err != nil {
		t.Fatal(err)
	}

	if len(challenge.Challenge) != 32 {
		t.Errorf("expected 32-byte challenge, got %d", len(challenge.Challenge))
	}

	if challenge.Verified {
		t.Error("new challenge should not be verified")
	}

	if challenge.ExpiresAt.Before(time.Now()) {
		t.Error("challenge should not be expired immediately")
	}

	// Get the challenge back.
	fetched, err := store.Get(ctx, challenge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched == nil {
		t.Fatal("expected to find challenge")
	}
	if fetched.ID != challenge.ID {
		t.Errorf("expected ID %s, got %s", challenge.ID, fetched.ID)
	}
}

func TestMockChallengeStore_GetNonExistent(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()

	fetched, err := store.Get(ctx, types.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if fetched != nil {
		t.Error("expected nil for non-existent challenge")
	}
}

func TestMockChallengeStore_Verify(t *testing.T) {
	store := newMockStore()
	ctx := context.Background()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	challenge, _ := store.Create(ctx, pub)

	if err := store.Verify(ctx, challenge.ID); err != nil {
		t.Fatal(err)
	}

	fetched, _ := store.Get(ctx, challenge.ID)
	if !fetched.Verified {
		t.Error("challenge should be verified after Verify()")
	}
}

func TestChallengeHex(t *testing.T) {
	c := &Challenge{
		Challenge: []byte{0x00, 0x01, 0x02, 0xff},
	}
	hex := c.ChallengeHex()
	if hex != "000102ff" {
		t.Errorf("expected '000102ff', got %q", hex)
	}
}
