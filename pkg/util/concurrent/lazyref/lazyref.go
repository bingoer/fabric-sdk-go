/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package lazyref

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/hyperledger/fabric-sdk-go/pkg/common/logging"
)

var logger = logging.NewLogger("fabsdk/util")

// Initializer is a function that initializes the value
type Initializer func() (interface{}, error)

// Finalizer is a function that is called when the reference
// is closed
type Finalizer func(value interface{})

// ExpirationProvider is a function that returns the
// expiration time of a reference
type ExpirationProvider func() time.Duration

// valueHolder holds the actual value
type valueHolder struct {
	value interface{}
}

// expirationHandler is invoked when the
// reference expires
type expirationHandler func()

// ExpirationType indicates how to handle expiration of the reference
type ExpirationType uint

const (
	// LastAccessed specifies that the expiration time is calculated
	// from the last access time
	LastAccessed ExpirationType = iota

	// LastInitialized specifies that the expiration time is calculated
	// from the time the reference was initialized
	LastInitialized

	// Refreshing indicates that the reference should be periodically refreshed
	Refreshing
)

// Reference holds a value that is initialized on first access using the provided
// Initializer function. The Reference has an optional expiring feature
// wherin the value is reset after the provided period of time. A subsequent call
// to Get or MustGet causes the Initializer function to be invoked again.
// The Reference also has a proactive refresh capability, in which the Initializer
// function is periodically called out of band (out of band means that the caller
// of Get or MustGet does not need to wait for the initializer function to complete:
// the old value will be used until the new value has finished initializing).
// An optional Finalizer function may be provided to be invoked whenever the Reference
// is closed (via a call to Close) or if it expires. (Note: The Finalizer function
// is not called every time the value is refreshed with the periodic refresh feature.)
type Reference struct {
	lock               sync.RWMutex
	ref                unsafe.Pointer
	lastTimeAccessed   unsafe.Pointer
	initializer        Initializer
	finalizer          Finalizer
	expirationHandler  expirationHandler
	expirationProvider ExpirationProvider
	initialInit        time.Duration
	expiryType         ExpirationType
	closed             bool
	closech            chan bool
	running            bool
	wg                 sync.WaitGroup
}

// New creates a new reference
func New(initializer Initializer, opts ...Opt) *Reference {
	lazyRef := &Reference{
		initializer: initializer,
		initialInit: InitOnFirstAccess,
	}

	for _, opt := range opts {
		opt(lazyRef)
	}

	if lazyRef.expirationProvider != nil {
		// This is an expiring reference. After the initializer is
		// called, set a timer that will call the expiration handler.
		initializer := lazyRef.initializer
		initialExpiration := lazyRef.expirationProvider()
		lazyRef.initializer = func() (interface{}, error) {
			value, err := initializer()
			if err == nil {
				lazyRef.ensureTimerStarted(initialExpiration)
			}
			return value, err
		}

		lazyRef.closech = make(chan bool, 1)

		if lazyRef.expirationHandler == nil {
			// Set a default expiration handler
			lazyRef.expirationHandler = lazyRef.resetValue
		}
		if lazyRef.initialInit >= 0 {
			lazyRef.ensureTimerStarted(lazyRef.initialInit)
		}
	}

	return lazyRef
}

// Get returns the value, or an error if the initialiser returned an error.
func (r *Reference) Get() (interface{}, error) {
	// Try outside of a lock
	if value, ok := r.get(); ok {
		return value, nil
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	if r.closed {
		return nil, errors.New("reference is already closed")
	}

	// Try again inside the lock
	if value, ok := r.get(); ok {
		return value, nil
	}

	// Value hasn't been set yet

	value, err := r.initializer()
	if err != nil {
		return nil, err
	}
	r.set(value)

	return value, nil
}

// MustGet returns the value. If an error is returned
// during initialization of the value then this function
// will panic.
func (r *Reference) MustGet() interface{} {
	value, err := r.Get()
	if err != nil {
		panic(fmt.Sprintf("error returned from Get: %s", err))
	}
	return value
}

// Close ensures that the finalizer (if provided) is called.
// Close should be called for expiring references and
// rerences that specify finalizers.
func (r *Reference) Close() {
	if !r.setClosed() {
		// Already closed
		return
	}

	logger.Debug("Closing reference")

	r.notifyClosing()
	r.wg.Wait()
	r.finalize()
}

func (r *Reference) setClosed() bool {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return false
	}
	r.closed = true
	return true
}

func (r *Reference) notifyClosing() {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.running {
		logger.Debugf("Sending closed event...")
		r.closech <- true
	}
}

func (r *Reference) get() (interface{}, bool) {
	r.setLastAccessed()
	p := atomic.LoadPointer(&r.ref)
	if p == nil {
		return nil, false
	}
	return (*valueHolder)(p).value, true
}

func (r *Reference) isSet() bool {
	return atomic.LoadPointer(&r.ref) != nil
}

func (r *Reference) set(value interface{}) {
	atomic.StorePointer(&r.ref, unsafe.Pointer(&valueHolder{value: value}))
}

func (r *Reference) setLastAccessed() {
	now := time.Now()
	atomic.StorePointer(&r.lastTimeAccessed, unsafe.Pointer(&now))
}

func (r *Reference) lastAccessed() time.Time {
	p := atomic.LoadPointer(&r.lastTimeAccessed)
	return *(*time.Time)(p)
}

func (r *Reference) timerRunning() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.running
}

func (r *Reference) setTimerRunning() bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.running || r.closed {
		logger.Debugf("Cannot start timer since timer is either already running or it is closed")
		return false
	}

	r.running = true
	r.wg.Add(1)
	logger.Debugf("Timer started")
	return true
}

func (r *Reference) setTimerStopped() {
	r.lock.Lock()
	defer r.lock.Unlock()
	logger.Debugf("Timer stopped")
	r.running = false
	r.wg.Done()
}

func (r *Reference) ensureTimerStarted(initialExpiration time.Duration) {
	if r.running {
		logger.Debugf("Timer is already running")
		return
	}

	r.setLastAccessed()

	go func() {
		if !r.setTimerRunning() {
			logger.Debugf("Timer is already running")
			return
		}
		defer r.setTimerStopped()

		logger.Debugf("Starting timer")

		expiry := initialExpiration
		for {
			select {
			case <-r.closech:
				logger.Debugf("Got closed event. Exiting timer.")
				return

			case <-time.After(expiry):
				expiration := r.expirationProvider()

				if !r.isSet() && r.expiryType != Refreshing {
					expiry = expiration
					logger.Debugf("Reference is not set. Will expire again in %s", expiry)
					continue
				}

				if r.expiryType == LastInitialized || r.expiryType == Refreshing {
					logger.Debugf("Handling expiration...")
					r.handleExpiration()
					expiry = expiration
					logger.Debugf("... finished handling expiration. Setting expiration to %s", expiry)
				} else {
					// Check how long it's been since last access
					durSinceLastAccess := time.Now().Sub(r.lastAccessed())
					logger.Debugf("Duration since last access is %s", durSinceLastAccess)
					if durSinceLastAccess > expiration {
						logger.Debugf("... handling expiration...")
						r.handleExpiration()
						expiry = expiration
						logger.Debugf("... finished handling expiration. Setting expiration to %s", expiry)
					} else {
						// Set another expiry for the remainder of the time
						expiry = expiration - durSinceLastAccess
						logger.Debugf("Not expired yet. Will check again in %s", expiry)
					}
				}
			}
		}
	}()
}

func (r *Reference) finalize() {
	if r.finalizer == nil {
		return
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	if r.isSet() {
		value, _ := r.get()
		r.finalizer(value)
	}
}

func (r *Reference) handleExpiration() {
	r.lock.Lock()
	defer r.lock.Unlock()

	logger.Debug("Invoking expiration handler")
	r.expirationHandler()
}

// resetValue is an expiration handler that calls the
// finalizer and resets the reference to nil.
// Note: This function is invoked from inside a write
// lock so there's no need to lock
func (r *Reference) resetValue() {
	if r.finalizer != nil {
		value, _ := r.get()
		r.finalizer(value)
	}
	atomic.StorePointer(&r.ref, nil)
}

// refreshValue is an expiration handler that calls the
// initializer and, if the initializer was successful, resets
// the reference with the new value.
// Note: This function is invoked from inside a write
// lock so there's no need to lock
func (r *Reference) refreshValue() {
	if value, err := r.initializer(); err != nil {
		logger.Warnf("Error - initializer returned error: %s. Will retry again later", err)
	} else {
		r.set(value)
	}
}
