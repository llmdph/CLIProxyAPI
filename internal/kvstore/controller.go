package kvstore

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// localRecoveryInterval is how often a failed local bbolt open is retried in
// the background. A transient flock collision (for example a canary instance
// that briefly shares the same auth directory during a rollout) must not pin
// the KV store in a permanently unavailable state.
const localRecoveryInterval = 2 * time.Second

var (
	controllerMu sync.RWMutex
	homeProvider func() (KVClient, bool, error)
	localEnabled bool
	localStore   *BoltStore
	localPath    string
	localErr     error
	localRecover chan struct{}
)

// RegisterHomeProvider registers the callback used by CurrentKVClient to inspect
// whether Home mode is active and obtain the active Home KV client.
func RegisterHomeProvider(fn func() (KVClient, bool, error)) {
	controllerMu.Lock()
	homeProvider = fn
	controllerMu.Unlock()
}

// ConfigureLocal enables, reconfigures, or disables the local bbolt-backed KV store.
func ConfigureLocal(enabled bool, rawPath string) error {
	controllerMu.Lock()
	defer controllerMu.Unlock()

	stopLocalRecoveryLocked()

	if !enabled {
		return shutdownLocalLocked()
	}

	resolvedPath, errResolve := ResolvePath(rawPath)
	if errResolve != nil {
		if localStore != nil {
			if errClose := localStore.Close(); errClose != nil {
				log.Errorf("local kv store close failed: %v", errClose)
			}
			localStore = nil
			localPath = ""
		}
		localEnabled = true
		localErr = errResolve
		log.Errorf("local kv store resolve path failed: %v", errResolve)
		return errResolve
	}

	if localStore != nil && localPath == resolvedPath && localErr == nil {
		localEnabled = true
		return nil
	}

	if localStore != nil {
		if errClose := localStore.Close(); errClose != nil {
			log.Errorf("local kv store close failed: %v", errClose)
		}
		localStore = nil
		localPath = ""
	}

	store, errOpen := OpenBoltStore(resolvedPath)
	if errOpen != nil {
		localEnabled = true
		localPath = resolvedPath
		localErr = errOpen
		log.Errorf("local kv store open failed (path=%s): %v", resolvedPath, errOpen)
		startLocalRecoveryLocked()
		return errOpen
	}

	localEnabled = true
	localStore = store
	localPath = resolvedPath
	localErr = nil
	return nil
}

// ShutdownLocal closes any open local BoltStore and resets local KV state to disabled.
func ShutdownLocal() error {
	controllerMu.Lock()
	defer controllerMu.Unlock()
	return shutdownLocalLocked()
}

func shutdownLocalLocked() error {
	stopLocalRecoveryLocked()
	var errClose error
	if localStore != nil {
		errClose = localStore.Close()
	}
	localEnabled = false
	localStore = nil
	localPath = ""
	localErr = nil
	return errClose
}

// startLocalRecoveryLocked launches a single background retry loop for a local
// store that failed to open. The loop exits as soon as the store opens, the
// configuration changes, or the store is shut down.
func startLocalRecoveryLocked() {
	if localRecover != nil || localPath == "" {
		return
	}
	stop := make(chan struct{})
	localRecover = stop
	path := localPath
	go func() {
		ticker := time.NewTicker(localRecoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}

			store, errOpen := OpenBoltStore(path)

			controllerMu.Lock()
			if localRecover != stop {
				controllerMu.Unlock()
				if store != nil {
					if errClose := store.Close(); errClose != nil {
						log.Errorf("local kv store recovery close failed: %v", errClose)
					}
				}
				return
			}
			if errOpen != nil {
				localErr = errOpen
				controllerMu.Unlock()
				continue
			}
			localStore = store
			localErr = nil
			localRecover = nil
			controllerMu.Unlock()
			log.Infof("local kv store recovered (path=%s)", path)
			return
		}
	}()
}

func stopLocalRecoveryLocked() {
	if localRecover != nil {
		close(localRecover)
		localRecover = nil
	}
}

// CurrentKVClient returns the active KVClient using the three-tier routing order:
//  1. Home KV when Home mode is active (never falling back to local bbolt).
//  2. Local BoltStore when server.kv-store.disabled is false.
//  3. (nil, false, nil) for default in-memory storage.
func CurrentKVClient() (KVClient, bool, error) {
	controllerMu.RLock()
	provider := homeProvider
	controllerMu.RUnlock()

	if provider != nil {
		if client, homeMode, errHome := provider(); homeMode {
			return client, true, errHome
		}
	}

	controllerMu.RLock()
	defer controllerMu.RUnlock()

	if !localEnabled {
		return nil, false, nil
	}
	if localErr != nil {
		return nil, true, fmt.Errorf("local kv store unavailable: %w", localErr)
	}
	if localStore == nil {
		return nil, true, fmt.Errorf("local kv store unavailable: %w", errStoreClosed)
	}
	return localStore, true, nil
}
