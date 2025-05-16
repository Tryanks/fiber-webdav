package webdav

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/emersion/go-webdav/internal"
)

// LockSystem provides an in-memory implementation of WebDAV locks.
type LockSystem struct {
	mu    sync.RWMutex
	locks map[string]*lockInfo // Map of token -> lock info
	paths map[string][]string  // Map of path -> tokens
}

// lockInfo contains information about an active lock.
type lockInfo struct {
	Token   string
	Root    string
	Created time.Time
	Timeout time.Duration
}

// Global lock system that can be used by all backends
var globalLockSystem *LockSystem

// NewLockSystem creates a new in-memory lock system.
func NewLockSystem() *LockSystem {
	return &LockSystem{
		locks: make(map[string]*lockInfo),
		paths: make(map[string][]string),
	}
}

// GetGlobalLockSystem returns the global lock system, creating it if necessary.
func GetGlobalLockSystem() *LockSystem {
	if globalLockSystem == nil {
		globalLockSystem = NewLockSystem()
	}
	return globalLockSystem
}

// Lock creates or refreshes a lock.
func (ls *LockSystem) Lock(r *http.Request, depth internal.Depth, timeout time.Duration, refreshToken string) (*internal.Lock, bool, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	path := r.URL.Path

	// If refreshToken is provided, refresh the existing lock
	if refreshToken != "" {
		lock, ok := ls.locks[refreshToken]
		if !ok {
			return nil, false, internal.HTTPErrorf(http.StatusPreconditionFailed, "webdav: lock token not found")
		}

		// Update the timeout
		lock.Timeout = timeout
		lock.Created = time.Now()

		return &internal.Lock{
			Href:    lock.Token,
			Root:    lock.Root,
			Timeout: lock.Timeout,
		}, false, nil
	}

	// Check if the path is already locked
	if tokens, ok := ls.paths[path]; ok && len(tokens) > 0 {
		return nil, false, internal.HTTPErrorf(http.StatusLocked, "webdav: path already locked")
	}

	// Create a new lock
	token := generateToken()
	lock := &lockInfo{
		Token:   token,
		Root:    path,
		Created: time.Now(),
		Timeout: timeout,
	}

	// Store the lock
	ls.locks[token] = lock
	ls.paths[path] = append(ls.paths[path], token)

	return &internal.Lock{
		Href:    token,
		Root:    path,
		Timeout: timeout,
	}, true, nil
}

// Unlock removes a lock.
func (ls *LockSystem) Unlock(r *http.Request, tokenHref string) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	lock, ok := ls.locks[tokenHref]
	if !ok {
		return internal.HTTPErrorf(http.StatusPreconditionFailed, "webdav: lock token not found")
	}

	// Remove the lock from the paths map
	path := lock.Root
	tokens := ls.paths[path]
	for i, t := range tokens {
		if t == tokenHref {
			// Remove the token from the slice
			ls.paths[path] = append(tokens[:i], tokens[i+1:]...)
			break
		}
	}

	// If the path has no more locks, remove it from the map
	if len(ls.paths[path]) == 0 {
		delete(ls.paths, path)
	}

	// Remove the lock from the locks map
	delete(ls.locks, tokenHref)

	return nil
}

// CleanExpiredLocks removes expired locks.
func (ls *LockSystem) CleanExpiredLocks() {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	now := time.Now()
	for token, lock := range ls.locks {
		// Skip infinite locks
		if lock.Timeout == 0 {
			continue
		}

		// Check if the lock has expired
		if now.Sub(lock.Created) > lock.Timeout {
			// Remove the lock from the paths map
			path := lock.Root
			tokens := ls.paths[path]
			for i, t := range tokens {
				if t == token {
					// Remove the token from the slice
					ls.paths[path] = append(tokens[:i], tokens[i+1:]...)
					break
				}
			}

			// If the path has no more locks, remove it from the map
			if len(ls.paths[path]) == 0 {
				delete(ls.paths, path)
			}

			// Remove the lock from the locks map
			delete(ls.locks, token)
		}
	}
}

// generateToken creates a unique token for a lock.
func generateToken() string {
	// Create a simple unique token using timestamp and random number
	return fmt.Sprintf("opaquelocktoken:%d-%d", time.Now().UnixNano(), time.Now().Unix())
}
