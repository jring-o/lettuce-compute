package cli

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/spf13/cobra"
)

func newProveIdentityCmd() *cobra.Command {
	var challenge string

	cmd := &cobra.Command{
		Use:   "prove-identity",
		Short: "Sign a challenge to prove ownership of this volunteer identity",
		Long: `Signs a hex-encoded challenge with the volunteer's Ed25519 private key.
Used for generic challenge-response identity verification.

The external verifier provides a challenge hex string. This command signs it
and outputs a base64url-encoded signature that the verifier can submit for
verification against the infrastructure server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProveIdentity(challenge)
		},
	}

	cmd.Flags().StringVar(&challenge, "challenge", "", "hex-encoded challenge bytes to sign")
	_ = cmd.MarkFlagRequired("challenge")

	return cmd
}

func runProveIdentity(challengeHex string) error {
	// Decode hex challenge to bytes.
	challengeBytes, err := hex.DecodeString(challengeHex)
	if err != nil {
		return fmt.Errorf("invalid challenge hex: %w", err)
	}

	// Load private key from ~/.lettuce/ (uses global cfg from root command).
	privPath := filepath.Join(cfg.DataDir, "identity.key")
	pubPath := filepath.Join(cfg.DataDir, "identity.pub")

	pub, priv, err := identity.LoadKeyPair(privPath, pubPath)
	if err != nil {
		return fmt.Errorf("loading keypair: %w\n  Run 'lettuce-volunteer init' to generate a keypair.", err)
	}

	// Sign with Ed25519.
	signature := ed25519.Sign(priv, challengeBytes)

	// Output signature and public key.
	sigB64 := base64.RawURLEncoding.EncodeToString(signature)
	pubB64 := identity.PublicKeyToBase64URL(pub)

	fmt.Printf("Public Key:  %s\n", pubB64)
	fmt.Printf("Signature:   %s\n", sigB64)

	return nil
}
