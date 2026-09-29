package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
)

var (
	settingsBucket    = []byte("settings")
	nodesBucket       = []byte("nodes")
	credentialsBucket = []byte("credentials")
	tunnelsBucket     = []byte("tunnels")
	auditBucket       = []byte("audit")
	metaBucket        = []byte("meta")
	auditSeqKey       = []byte("audit_sequence")
)

type Store struct {
	db  *bolt.DB
	mu  sync.RWMutex
	key []byte
}

func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".npa"), nil
}

func Open(dataDir string) (*Store, error) {
	if dataDir == "" {
		var err error
		dataDir, err = DefaultDataDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dataDir, "state.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(dataDir, "master.key"))
	if err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{db: db, key: key}
	if err := store.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if raw, err := os.ReadFile(path); err == nil && len(raw) == 32 {
		return raw, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) initialize() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{settingsBucket, nodesBucket, credentialsBucket, tunnelsBucket, auditBucket, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Close() error { return s.db.Close() }

func writeJSON(bucket *bolt.Bucket, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, raw)
}

func readJSON(bucket *bolt.Bucket, key []byte, target any) bool {
	raw := bucket.Get(key)
	return raw != nil && json.Unmarshal(raw, target) == nil
}

func defaults() map[string]string {
	return map[string]string{
		"mcp_enabled":          "true",
		"mcp_ssh":              "false",
		"mcp_file":             "false",
		"mcp_tunnel":           "false",
		"allow_public_targets": "false",
		"log_level":            "info",
		"audit_retention_days": "30",
		"restore_tunnels":      "true",
	}
}

func (s *Store) Settings() (map[string]string, error) {
	result := defaults()
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(settingsBucket).ForEach(func(k, v []byte) error {
			result[string(k)] = string(v)
			return nil
		})
	})
	return result, err
}

func (s *Store) SaveSettings(values map[string]string) error {
	allowed := defaults()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(settingsBucket)
		for key, value := range values {
			if _, ok := allowed[key]; !ok {
				return apperrors.New(apperrors.InvalidArgument, "unknown setting: "+key)
			}
			if err := bucket.Put([]byte(key), []byte(value)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListNodes() ([]model.Node, error) {
	nodes := []model.Node{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(nodesBucket).ForEach(func(_, value []byte) error {
			var node model.Node
			if err := json.Unmarshal(value, &node); err != nil {
				return err
			}
			nodes = append(nodes, node)
			return nil
		})
	})
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, err
}

func (s *Store) GetNode(id string) (model.Node, error) {
	var node model.Node
	err := s.db.View(func(tx *bolt.Tx) error {
		if !readJSON(tx.Bucket(nodesBucket), []byte(id), &node) {
			return apperrors.New(apperrors.InvalidArgument, "node not found")
		}
		return nil
	})
	return node, err
}

func (s *Store) SaveNode(node model.Node) error {
	if node.ID == "" || node.Name == "" || node.Address == "" || node.Username == "" {
		return apperrors.New(apperrors.InvalidArgument, "id, name, address and username are required")
	}
	if node.Port <= 0 || node.Port > 65535 {
		return apperrors.New(apperrors.InvalidArgument, "invalid SSH port")
	}
	if node.AuthMethod != "key" && node.AuthMethod != "password" && node.AuthMethod != "agent" {
		return apperrors.New(apperrors.InvalidArgument, "auth_method must be key, password or agent")
	}
	if node.Protocol == "" {
		node.Protocol = "sftp"
	}
	if node.Status == "" {
		node.Status = "unknown"
	}
	now := time.Now().UTC()
	if node.CreatedAt.IsZero() {
		node.CreatedAt = now
	}
	node.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(nodesBucket), []byte(node.ID), node) })
}

func (s *Store) UpdateNodeStatus(id, status, lastError string) error {
	node, err := s.GetNode(id)
	if err != nil {
		return err
	}
	node.Status = status
	node.LastError = lastError
	node.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(nodesBucket), []byte(id), node) })
}

func (s *Store) DeleteNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(nodesBucket).Delete([]byte(id)); err != nil {
			return err
		}
		if err := tx.Bucket(credentialsBucket).Delete([]byte(id)); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) GetCredential(nodeID string) (model.Credential, error) {
	var encrypted struct {
		Nonce string `json:"n"`
		Data  string `json:"d"`
	}
	var credential model.Credential
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(credentialsBucket).Get([]byte(nodeID))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &encrypted); err != nil {
			return err
		}
		plain, err := decrypt(s.key, encrypted.Nonce, encrypted.Data)
		if err != nil {
			return err
		}
		return json.Unmarshal(plain, &credential)
	})
	return credential, err
}

func (s *Store) SaveCredential(credential model.Credential) error {
	plain, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	nonce, data, err := encrypt(s.key, plain)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]string{"n": nonce, "d": data})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(credentialsBucket).Put([]byte(credential.NodeID), raw) })
}

func encrypt(key, plaintext []byte) (string, string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	return base64.RawStdEncoding.EncodeToString(nonce), base64.RawStdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, nil)), nil
}

func decrypt(key []byte, nonceEncoded, dataEncoded string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(nonceEncoded)
	if err != nil {
		return nil, err
	}
	data, err := base64.RawStdEncoding.DecodeString(dataEncoded)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, data, nil)
}

func (s *Store) ListTunnels() ([]model.Tunnel, error) {
	tunnels := []model.Tunnel{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(tunnelsBucket).ForEach(func(_, value []byte) error {
			var tunnel model.Tunnel
			if err := json.Unmarshal(value, &tunnel); err != nil {
				return err
			}
			tunnels = append(tunnels, tunnel)
			return nil
		})
	})
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].Name < tunnels[j].Name })
	return tunnels, err
}

func (s *Store) GetTunnel(id string) (model.Tunnel, error) {
	var tunnel model.Tunnel
	err := s.db.View(func(tx *bolt.Tx) error {
		if !readJSON(tx.Bucket(tunnelsBucket), []byte(id), &tunnel) {
			return apperrors.New(apperrors.InvalidArgument, "tunnel not found")
		}
		return nil
	})
	return tunnel, err
}

func (s *Store) SaveTunnel(tunnel model.Tunnel) error {
	if tunnel.ID == "" || tunnel.NodeID == "" || tunnel.Name == "" {
		return apperrors.New(apperrors.InvalidArgument, "id, node_id and name are required")
	}
	switch tunnel.Type {
	case "local", "remote", "socks":
	default:
		return apperrors.New(apperrors.InvalidArgument, "type must be local, remote or socks")
	}
	if tunnel.ListenPort < 0 || tunnel.ListenPort > 65535 || tunnel.TargetPort < 0 || tunnel.TargetPort > 65535 {
		return apperrors.New(apperrors.InvalidArgument, "invalid port")
	}
	if tunnel.Type != "socks" && (tunnel.TargetHost == "" || tunnel.TargetPort == 0) {
		return apperrors.New(apperrors.InvalidArgument, "target host and port are required")
	}
	if tunnel.ListenAddress == "" {
		tunnel.ListenAddress = "127.0.0.1"
	}
	if tunnel.ListenPort == 0 && tunnel.Type == "local" {
		tunnel.ListenPort = 0
	}
	if tunnel.Status == "" {
		tunnel.Status = "stopped"
	}
	now := time.Now().UTC()
	if tunnel.CreatedAt.IsZero() {
		tunnel.CreatedAt = now
	}
	tunnel.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(tunnelsBucket), []byte(tunnel.ID), tunnel) })
}

func (s *Store) UpdateTunnelStatus(id, status, lastError string) error {
	tunnel, err := s.GetTunnel(id)
	if err != nil {
		return err
	}
	tunnel.Status = status
	tunnel.LastError = lastError
	tunnel.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(tunnelsBucket), []byte(id), tunnel) })
}

func (s *Store) DeleteTunnel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(tunnelsBucket).Delete([]byte(id)) })
}

func (s *Store) AddAudit(event model.AuditEvent) (model.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		var sequence uint64
		if raw := meta.Get(auditSeqKey); raw != nil {
			_, _ = fmt.Sscan(string(raw), &sequence)
		}
		sequence++
		event.ID = int64(sequence)
		if err := meta.Put(auditSeqKey, []byte(fmt.Sprintf("%d", sequence))); err != nil {
			return err
		}
		key, err := json.Marshal(event.ID)
		if err != nil {
			return err
		}
		return writeJSON(tx.Bucket(auditBucket), key, event)
	})
	return event, err
}

func (s *Store) ListAudit(limit, offset int) ([]model.AuditEvent, error) {
	events := []model.AuditEvent{}
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(auditBucket).Cursor()
		for k, v := cursor.Last(); k != nil; k, v = cursor.Prev() {
			if offset > 0 {
				offset--
				continue
			}
			var event model.AuditEvent
			if err := json.Unmarshal(v, &event); err != nil {
				return err
			}
			events = append(events, event)
			if len(events) >= limit {
				break
			}
		}
		return nil
	})
	return events, err
}

func (s *Store) PruneAudit(before time.Time) (int, error) {
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(auditBucket)
		var keys [][]byte
		err := bucket.ForEach(func(k, v []byte) error {
			var event model.AuditEvent
			if json.Unmarshal(v, &event) == nil && event.Time.Before(before) {
				key := append([]byte(nil), k...)
				keys = append(keys, key)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := bucket.Delete(key); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}
