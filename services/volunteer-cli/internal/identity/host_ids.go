package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Host identity is HEAD-ISSUED (BG-25). The head mints a per-machine host id at
// registration and returns it in RegisterVolunteerResponse.host_id; the client
// persists exactly that value and echoes it on subsequent contact with THAT head.
// Clients no longer self-generate host ids.
//
// Ids are PER-HEAD values (audit F-2): the same machine presents a different
// head-issued id to each head it talks to, so a single shared file would clobber
// one head's id with another's. HostIDStore persists them as a JSON object keyed by
// the head's stable identifier — the gRPC address (config.ServerConfig.GRPCAddress).
// The address is the natural per-head key: the daemon opens exactly one connection
// per address (start.go dedups on it), and every per-head lookup in the daemon keys
// on it, whereas a head's display name is cosmetic and may change. The file lives at
// <DataDir>/host-ids.json.
//
// The legacy single-file host id (<DataDir>/host.id, the retired client-generated
// scheme) is deliberately IGNORED: it is neither read nor migrated. The head no
// longer honors a client-generated id, so migrating one would only seed a value the
// head immediately refuses; a fresh head-issued id is minted on first registration
// instead. Any existing host.id file is left on disk untouched.

// HostIDStore persists head-issued host ids keyed by head (gRPC address). It is
// safe for concurrent use within one process: every mutation is a read-modify-write
// under the mutex. Only one daemon runs at a time (PID lock), so no cross-process
// coordination is needed.
type HostIDStore struct {
	path string
	mu   sync.Mutex
}

// NewHostIDStore returns a store backed by the given JSON file. The file is created
// lazily on the first Set.
func NewHostIDStore(path string) *HostIDStore {
	return &HostIDStore{path: path}
}

// Get returns the stored host id for headKey, or "" if none is stored. A missing
// backing file is not an error (it simply means no id has been persisted yet).
func (s *HostIDStore) Get(headKey string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return "", err
	}
	return m[headKey], nil
}

// Set persists hostID as the id for headKey, replacing any previous value. Passing
// an empty hostID deletes the entry (the register flow calls Delete directly for
// that case; Set treats empty as delete so it can never write a blank id).
func (s *HostIDStore) Set(headKey, hostID string) error {
	if hostID == "" {
		return s.Delete(headKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	m[headKey] = hostID
	return s.save(m)
}

// All returns a copy of every stored head->id mapping. Used for diagnostics
// (`doctor`); mutating the returned map does not affect the store.
func (s *HostIDStore) All() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Delete removes any stored id for headKey. Deleting an absent key is a no-op and
// not an error.
func (s *HostIDStore) Delete(headKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := m[headKey]; !ok {
		return nil
	}
	delete(m, headKey)
	return s.save(m)
}

// load reads the backing file into a map. A missing file yields an empty map with
// no error; a malformed file returns an error so a mutation does not silently clobber
// other heads' ids.
func (s *HostIDStore) load() (map[string]string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading host ids: %w", err)
	}
	m := map[string]string{}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing host ids %s: %w", s.path, err)
	}
	return m, nil
}

// save writes the map back atomically-ish: it writes the whole file (0644),
// creating the parent dir (0700) as needed. Host ids are not secret (an owner
// spoofing their own machine id gains nothing — credit is validated-output and
// quota is measured), so 0644 matches the retired host.id file's mode.
func (s *HostIDStore) save(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("creating directory for host ids: %w", err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling host ids: %w", err)
	}
	if err := os.WriteFile(s.path, b, 0644); err != nil {
		return fmt.Errorf("writing host ids: %w", err)
	}
	return nil
}
