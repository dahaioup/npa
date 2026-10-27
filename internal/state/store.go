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
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	apperrors "github.com/npa/npa/internal/errors"
	"github.com/npa/npa/internal/model"
)

var (
	settingsBucket     = []byte("settings")
	nodesBucket        = []byte("nodes")
	credentialsBucket  = []byte("credentials")
	tunnelsBucket      = []byte("tunnels")
	auditBucket        = []byte("audit")
	discoveredBucket   = []byte("discovered_nodes")
	relayBucket        = []byte("relay_routes")
	portMappingsBucket = []byte("port_mappings")
	metaBucket         = []byte("meta")
	auditSeqKey        = []byte("audit_sequence")
)

type Store struct {
	db      *bolt.DB
	mu      sync.RWMutex
	key     []byte
	dataDir string
}

// DataDir 返回状态库所在目录，日志、更新缓存与自启动标记都放在它下面。
func (s *Store) DataDir() string {
	if s == nil || s.dataDir == "" {
		if dir, err := DefaultDataDir(); err == nil {
			return dir
		}
		return "."
	}
	return s.dataDir
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
	store := &Store{db: db, key: key, dataDir: dataDir}
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
		for _, name := range [][]byte{settingsBucket, nodesBucket, credentialsBucket, tunnelsBucket, auditBucket, discoveredBucket, relayBucket, portMappingsBucket, metaBucket} {
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
		// 调试期默认：能力全开、MCP 与 Discovery 局域网可达（Web 管理面仍固定回环）。
		"mcp_enabled":               "true",
		"mcp_ssh":                   "true",
		"mcp_file":                  "true",
		"mcp_tunnel":                "true",
		"mcp_screenshot":            "true",
		"mcp_mouse":                 "true",
		"mcp_discovery":             "true",
		"mcp_relay":                 "true",
		"mcp_bind_scope":            "lan",
		"discovery_enabled":         "true",
		"allow_public_targets":      "false",
		"log_level":                 "info",
		"audit_retention_days":      "30",
		"restore_tunnels":           "true",
		"restore_port_mappings":     "true",
		"peer_enabled":              "true",
		"peer_bind_scope":           "lan",
		"peer_port":                 "56701",
		"peer_seeds":                "",
		"peer_probe_subnets":        "",
		"peer_auto_discover_uppers": "true", // 无种子时走路由探测自动找上级网络
		"peer_pool_size":            "4",
		"peer_auto_trust":           "true",
		"autostart":                 "false",
		"hide_console":              "false",
		"update_artifact":           "",
		"push_package":              "",
		"peer_uppers":               "",
		"peer_advertise":            "",
		"peer_on_demand_idle":       "60",
		"peer_relay_fallback":       "true",
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
	if node.Port < 0 || node.Port > 65535 {
		return apperrors.New(apperrors.InvalidArgument, "invalid SSH port")
	}
	// 反向接入的 peer 节点不需要 SSH 端口（连接由它主动建立），地址也只是自报值。
	if node.Protocol != "peer" && node.Port <= 0 {
		return apperrors.New(apperrors.InvalidArgument, "invalid SSH port")
	}
	if node.Port > 65535 {
		return apperrors.New(apperrors.InvalidArgument, "invalid SSH port")
	}
	switch node.AuthMethod {
	case "key", "password", "agent", "peer":
	default:
		return apperrors.New(apperrors.InvalidArgument, "auth_method must be key, password, agent or peer")
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

func (s *Store) ListDiscoveredNodes() ([]model.Node, error) {
	nodes := []model.Node{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(discoveredBucket).ForEach(func(_, value []byte) error {
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

func (s *Store) SaveDiscoveredNode(node model.Node) error {
	if node.ID == "" {
		return apperrors.New(apperrors.InvalidArgument, "discovered node id is required")
	}
	node.Discovered = true
	node.UpdatedAt = time.Now().UTC()
	if node.LastSeen.IsZero() {
		node.LastSeen = node.UpdatedAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(discoveredBucket), []byte(node.ID), node) })
}

func (s *Store) DeleteDiscoveredNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(discoveredBucket).Delete([]byte(id)) })
}

func (s *Store) ListRelayRoutes() ([]model.RelayRoute, error) {
	routes := []model.RelayRoute{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(relayBucket).ForEach(func(_, value []byte) error {
			var route model.RelayRoute
			if err := json.Unmarshal(value, &route); err != nil {
				return err
			}
			routes = append(routes, route)
			return nil
		})
	})
	return routes, err
}

func (s *Store) SaveRelayRoute(route model.RelayRoute) error {
	if route.TargetSubnet == "" || route.ViaNodeID == "" {
		return apperrors.New(apperrors.InvalidArgument, "relay route target and node are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		return writeJSON(tx.Bucket(relayBucket), []byte(route.TargetSubnet+"|"+route.ViaNodeID), route)
	})
}

func (s *Store) DeleteRelayRoute(targetSubnet, viaNodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(relayBucket).Delete([]byte(targetSubnet + "|" + viaNodeID)) })
}

func (s *Store) ListPortMappings() ([]model.PortMapping, error) {
	mappings := []model.PortMapping{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(portMappingsBucket).ForEach(func(_, value []byte) error {
			var mapping model.PortMapping
			if err := json.Unmarshal(value, &mapping); err != nil {
				return err
			}
			mappings = append(mappings, mapping)
			return nil
		})
	})
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].Name < mappings[j].Name })
	return mappings, err
}

func (s *Store) GetPortMapping(id string) (model.PortMapping, error) {
	var mapping model.PortMapping
	err := s.db.View(func(tx *bolt.Tx) error {
		if !readJSON(tx.Bucket(portMappingsBucket), []byte(id), &mapping) {
			return apperrors.New(apperrors.InvalidArgument, "port mapping not found")
		}
		return nil
	})
	return mapping, err
}

func (s *Store) SavePortMapping(mapping model.PortMapping) error {
	if mapping.ID == "" || mapping.Name == "" {
		return apperrors.New(apperrors.InvalidArgument, "id and name are required")
	}
	if strings.TrimSpace(mapping.TargetHost) == "" {
		return apperrors.New(apperrors.InvalidArgument, "目标主机不能为空")
	}
	if mapping.TargetPort <= 0 || mapping.TargetPort > 65535 {
		return apperrors.New(apperrors.InvalidArgument, "目标端口必须在 1-65535 之间")
	}
	if mapping.ListenPort < 0 || mapping.ListenPort > 65535 {
		return apperrors.New(apperrors.InvalidArgument, "本机监听端口必须在 0-65535 之间")
	}
	if mapping.ListenAddress == "" {
		mapping.ListenAddress = "127.0.0.1"
	}
	if mapping.ListenAddress != "127.0.0.1" && mapping.ListenAddress != "::1" && mapping.ListenAddress != "0.0.0.0" && mapping.ListenAddress != "::" {
		return apperrors.New(apperrors.InvalidArgument, "监听地址仅支持 127.0.0.1、::1、0.0.0.0 或 ::")
	}
	if mapping.Status == "" {
		mapping.Status = "stopped"
	}
	now := time.Now().UTC()
	if mapping.CreatedAt.IsZero() {
		mapping.CreatedAt = now
	}
	mapping.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(portMappingsBucket), []byte(mapping.ID), mapping) })
}

func (s *Store) UpdatePortMappingStatus(id, status, lastError string) error {
	mapping, err := s.GetPortMapping(id)
	if err != nil {
		return err
	}
	mapping.Status = status
	mapping.LastError = lastError
	mapping.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return writeJSON(tx.Bucket(portMappingsBucket), []byte(id), mapping) })
}

func (s *Store) DeletePortMapping(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(portMappingsBucket).Delete([]byte(id)) })
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
