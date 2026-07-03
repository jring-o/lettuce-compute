package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/atproto"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/spf13/cobra"
)

// defaultKeyAuthorizationCollection is the ATProto collection (NSID) the
// keyAuthorization record is written under. The namespace is pending an operator
// decision, so it is kept overridable via --collection.
const defaultKeyAuthorizationCollection = "tech.scios.lettuce.keyAuthorization"

// appPasswordEnv is the environment variable consulted for the PDS app password
// when --app-password is not supplied. The password is never written to disk.
const appPasswordEnv = "LETTUCE_ATPROTO_APP_PASSWORD"

func newBindDIDCmd() *cobra.Command {
	var (
		handle      string
		did         string
		pdsURL      string
		appPassword string
		label       string
		collection  string
		headURL     string
	)

	// Default the label to this machine's hostname so a volunteer running the
	// same account key on several machines can tell the bindings apart.
	defaultLabel, _ := os.Hostname()

	cmd := &cobra.Command{
		Use:   "bind-did",
		Short: "Bind this device's key to your ATProto DID via a PDS record",
		Long: `Publish a keyAuthorization record into your own ATProto Personal Data Server
(PDS) repository that binds this device's Ed25519 key to your ATProto DID, then
notify one or more Lettuce heads so they can verify the binding.

Authentication to the PDS uses a one-time app password. The app password is used
only to create a session for this command and is NEVER written to disk or config.
Supply it with --app-password, the ` + appPasswordEnv + ` environment variable, or
answer the interactive prompt.

Examples:
  lettuce-volunteer bind-did --handle alice.bsky.social
  lettuce-volunteer bind-did --did did:plc:abc123 --head-url https://head.example.com`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBindDID(cmd, bindDIDOptions{
				handle:      handle,
				did:         did,
				pdsURL:      pdsURL,
				appPassword: appPassword,
				label:       label,
				collection:  collection,
				headURL:     headURL,
			})
		},
	}

	cmd.Flags().StringVar(&handle, "handle", "", "your ATProto handle (e.g. alice.bsky.social); exactly one of --handle/--did is required")
	cmd.Flags().StringVar(&did, "did", "", "your ATProto DID (e.g. did:plc:...); exactly one of --handle/--did is required")
	cmd.Flags().StringVar(&pdsURL, "pds-url", "https://bsky.social", "base URL of your ATProto PDS")
	cmd.Flags().StringVar(&appPassword, "app-password", "", "PDS app password (falls back to "+appPasswordEnv+", then an interactive prompt); never persisted")
	cmd.Flags().StringVar(&label, "label", defaultLabel, "human-readable label for this device binding (default: hostname)")
	cmd.Flags().StringVar(&collection, "collection", defaultKeyAuthorizationCollection, "ATProto collection (NSID) to write the record under")
	cmd.Flags().StringVar(&headURL, "head-url", "", "head HTTP base URL to notify (e.g. https://head.example.com); if omitted, every attached head with a configured HTTP address is notified")

	return cmd
}

// bindDIDOptions carries the resolved flag values for runBindDID.
type bindDIDOptions struct {
	handle      string
	did         string
	pdsURL      string
	appPassword string
	label       string
	collection  string
	headURL     string
}

func runBindDID(cmd *cobra.Command, opts bindDIDOptions) error {
	ctx := cmd.Context()
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	// Exactly one of --handle / --did must be given.
	if (opts.handle == "") == (opts.did == "") {
		return fmt.Errorf("exactly one of --handle or --did is required")
	}

	// Determine which heads to notify before doing any network work, so a
	// misconfiguration fails fast.
	heads, err := resolveHeadURLs(opts.headURL)
	if err != nil {
		return err
	}

	// Load the device keypair (same location the daemon authenticates with).
	privPath := filepath.Join(cfg.DataDir, "identity.key")
	pubPath := filepath.Join(cfg.DataDir, "identity.pub")
	pub, priv, err := identity.LoadKeyPair(privPath, pubPath)
	if err != nil {
		return fmt.Errorf("loading keypair: %w\n  Run 'lettuce-volunteer init' to generate a keypair.", err)
	}

	operationalKey := atproto.EncodeEd25519DIDKey(pub)

	appPassword, err := resolveAppPassword(opts.appPassword, cmd.InOrStdin(), stderr)
	if err != nil {
		return err
	}

	identifier := opts.did
	if opts.handle != "" {
		identifier = opts.handle
	}

	// Create the PDS session. The app password lives only for this call.
	pds := atproto.NewClient(opts.pdsURL, nil)
	if err := pds.CreateSession(ctx, identifier, appPassword); err != nil {
		return fmt.Errorf("creating PDS session: %w", err)
	}

	// Resolve the authoritative DID. The DID is the permanent identity; the
	// handle is a mutable alias, so a handle bind is confirmed against the DID it
	// resolves to before anything is written.
	if opts.handle != "" {
		confirmed, err := confirmResolvedDID(cmd.InOrStdin(), stderr, pds.DID)
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		if !confirmed {
			return fmt.Errorf("aborted: DID %s not confirmed", pds.DID)
		}
	} else if pds.DID != opts.did {
		return fmt.Errorf("session DID %s does not match --did %s", pds.DID, opts.did)
	}
	boundDID := pds.DID

	// Sign the canonical binding bytes with the device key.
	createdAt := time.Now().UTC().Format(time.RFC3339)
	canonical, err := atproto.CanonicalBytes(boundDID, operationalKey, opts.label, createdAt)
	if err != nil {
		return err
	}
	keySignature := ed25519.Sign(priv, canonical)

	record := atproto.BuildRecord(opts.collection, boundDID, operationalKey, opts.label, createdAt, keySignature)

	// Write the record, overwriting an existing binding for this same device key
	// rather than accumulating duplicates.
	uri, cid, err := writeBindingRecord(ctx, pds, opts.collection, operationalKey, record)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "DID:         %s\n", boundDID)
	fmt.Fprintf(stdout, "Device key:  %s\n", operationalKey)
	fmt.Fprintf(stdout, "Record URI:  %s\n", uri)
	if cid != "" {
		fmt.Fprintf(stdout, "Record CID:  %s\n", cid)
	}

	// Notify each head. A head failure is reported but does not abort the others;
	// the record is already published and the binding can be re-notified.
	fmt.Fprintln(stdout, "\nNotifying heads:")
	var failures int
	for _, head := range heads {
		if err := atproto.NotifyHeadBindDID(ctx, nil, head, boundDID, uri, pub, priv, time.Now()); err != nil {
			failures++
			fmt.Fprintf(stdout, "  %s  FAILED: %v\n", head, err)
			continue
		}
		fmt.Fprintf(stdout, "  %s  verified\n", head)
	}

	fmt.Fprintln(stdout, "\nRun `lettuce-volunteer status` later to review your binding.")

	if failures > 0 {
		return fmt.Errorf("%d of %d head(s) failed to verify the binding", failures, len(heads))
	}
	return nil
}

// resolveHeadURLs returns the heads to notify. An explicit --head-url wins;
// otherwise every attached head with a configured HTTP address is used
// (deduplicated, since per-leaf entries repeat a head's address).
func resolveHeadURLs(headURL string) ([]string, error) {
	if headURL != "" {
		return []string{strings.TrimRight(headURL, "/")}, nil
	}

	seen := make(map[string]bool)
	var heads []string
	for _, srv := range cfg.Servers {
		addr := strings.TrimRight(srv.HTTPAddress, "/")
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		heads = append(heads, addr)
	}
	if len(heads) == 0 {
		return nil, fmt.Errorf("no head to notify: pass --head-url, or attach a head first with `lettuce-volunteer attach --server <host>`")
	}
	return heads, nil
}

// writeBindingRecord publishes the record, overwriting any existing record in the
// collection whose operationalKey matches (a prior binding of this same device
// key) and otherwise creating a new record.
func writeBindingRecord(ctx context.Context, pds *atproto.Client, collection, operationalKey string, record map[string]any) (uri, cid string, err error) {
	existing, err := pds.ListRecords(ctx, collection, 100)
	if err != nil {
		return "", "", fmt.Errorf("listing existing records: %w", err)
	}
	for _, r := range existing {
		if atproto.OperationalKeyOf(r.Value) != operationalKey {
			continue
		}
		rkey := atproto.RkeyFromURI(r.URI)
		if rkey == "" {
			continue
		}
		uri, cid, err = pds.PutRecord(ctx, collection, rkey, record)
		if err != nil {
			return "", "", fmt.Errorf("updating existing binding record: %w", err)
		}
		return uri, cid, nil
	}

	uri, cid, err = pds.CreateRecord(ctx, collection, record)
	if err != nil {
		return "", "", fmt.Errorf("creating binding record: %w", err)
	}
	return uri, cid, nil
}

// resolveAppPassword returns the PDS app password from, in order: the --app-password
// value, the environment variable, then an interactive prompt. The prompt reads a
// line from in and warns on warnOut that the input is not hidden, because the CLI
// has no terminal-echo-suppression dependency available.
func resolveAppPassword(flagValue string, in io.Reader, warnOut io.Writer) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv(appPasswordEnv); env != "" {
		return env, nil
	}

	fmt.Fprintln(warnOut, "WARNING: input is not hidden; the app password will be visible as you type.")
	fmt.Fprint(warnOut, "PDS app password: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("reading app password: %w", err)
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", fmt.Errorf("no app password provided")
	}
	return pw, nil
}

// confirmResolvedDID prints the DID a handle resolved to and returns true only if
// the user answers yes. It exists so a handle bind never proceeds against an
// unexpected DID (the DID is the permanent identity; the handle is an alias).
func confirmResolvedDID(in io.Reader, out io.Writer, did string) (bool, error) {
	fmt.Fprintf(out, "Handle resolved to DID: %s\n", did)
	fmt.Fprint(out, "This DID is the permanent identity that will be bound. Continue? [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
