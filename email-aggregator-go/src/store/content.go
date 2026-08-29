package store

import (
	"crypto/sha256"
	"fmt"
	"sync"
)

// StoredContent 内容存储回执
type StoredContent struct {
	ObjectKey   string `json:"objectKey"`
	ContentHash string `json:"contentHash"`
	Size        int    `json:"size"`
}

// ContentStore 内容存储接口（对象存储，内容寻址去重）
type ContentStore interface {
	// Put 写入内容，返回对象键与哈希（相同内容只存一份）
	Put(content []byte) (StoredContent, error)
	// Get 按对象键取回
	Get(objectKey string) ([]byte, error)
}

// InMemoryContentStore 演示/单测用：以 sha256 为键去重
type InMemoryContentStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewInMemoryContentStore 构造
func NewInMemoryContentStore() *InMemoryContentStore {
	return &InMemoryContentStore{objects: map[string][]byte{}}
}

// Put 写入（内容寻址）
func (s *InMemoryContentStore) Put(content []byte) (StoredContent, error) {
	sum := sha256.Sum256(content)
	hash := fmt.Sprintf("%x", sum)
	key := "mail/" + hash
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; !ok {
		s.objects[key] = content
	}
	return StoredContent{ObjectKey: key, ContentHash: hash, Size: len(content)}, nil
}

// Get 取回
func (s *InMemoryContentStore) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("content not found: %s", key)
	}
	return b, nil
}

// 真实对象存储实现见 integration/integration.go 中的 ObjectContentStore（//go:build integration）。
// 替换要点：用 minio-go / aws-sdk-go-v2，key = mail/<sha256>，存在则跳过。
