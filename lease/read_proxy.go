// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lease

import (
	"fmt"
	"io"

	"golang.org/x/net/context"
)

// A type used by read proxies to refresh their contents. See notes on
// NewReadProxy.
type Refresher interface {
	// Return the size of the underlying contents.
	Size() (size int64)

	// Return a read-closer for the contents. The same contents will always be
	// returned, and they will always be of length Size().
	Refresh(ctx context.Context) (rc io.ReadCloser, err error)
}

// A wrapper around a read lease, exposing a similar interface with the
// following differences:
//
//  *  Contents are fetched and re-fetched automatically when needed. Therefore
//     the user need not worry about lease expiration.
//
//  *  Methods that may involve fetching the contents (reading, seeking) accept
//     context arguments, so as to be cancellable.
//
//  *  Only random access reading is supported.
//
// External synchronization is required.
type ReadProxy interface {
	// Return the size of the proxied content. Guarantees to not block.
	Size() (size int64)

	// Semantics matching io.ReaderAt, except with context support and without
	// the guarantee of being thread-safe.
	ReadAt(ctx context.Context, p []byte, off int64) (n int, err error)

	// Return a read/write lease for the proxied contents, destroying the read
	// proxy. The read proxy must not be used after calling this method.
	Upgrade(ctx context.Context) (rwl ReadWriteLease, err error)

	// Destroy any resources in use by the read proxy. It must not be used
	// further.
	Destroy()

	// Panic if any internal invariants are violated.
	CheckInvariants()
}

// Create a read proxy.
//
// The supplied refresher will be used to obtain the proxy's contents whenever
// the file leaser decides to expire the temporary copy thus obtained.
//
// If rl is non-nil, it will be used as the first temporary copy of the
// contents, and must match what the refresher returns.
func NewReadProxy(
	fl FileLeaser,
	r Refresher,
	rl ReadLease) (rp ReadProxy) {
	rp = &readProxy{
		size:      r.Size(),
		leaser:    fl,
		refresher: r,
		lease:     rl,
	}

	return
}

// A wrapper around a read lease, exposing a similar interface with the
// following differences:
//
//  *  Contents are fetched and re-fetched automatically when needed. Therefore
//     the user need not worry about lease expiration.
//
//  *  Methods that may involve fetching the contents (reading, seeking) accept
//     context arguments, so as to be cancellable.
//
// External synchronization is required.
type readProxy struct {
	/////////////////////////
	// Constant data
	/////////////////////////

	size int64

	/////////////////////////
	// Dependencies
	/////////////////////////

	leaser    FileLeaser
	refresher Refresher

	/////////////////////////
	// Mutable state
	/////////////////////////

	// The current wrapped lease, or nil if one has never been issued.
	lease ReadLease
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

func isRevokedErr(err error) bool {
	_, ok := err.(*RevokedError)
	return ok
}

// Set up a read/write lease and fill in our contents.
//
// REQUIRES: The caller has observed that rp.lease has expired.
func (rp *readProxy) getContents(
	ctx context.Context) (rwl ReadWriteLease, err error) {
	// Obtain some space to write the contents.
	rwl, err = rp.leaser.NewFile()
	if err != nil {
		err = fmt.Errorf("NewFile: %v", err)
		return
	}

	// Clean up if we exit early.
	defer func() {
		if err != nil {
			rwl.Downgrade().Revoke()
		}
	}()

	// Obtain the reader for our contents.
	rc, err := rp.refresher.Refresh(ctx)
	if err != nil {
		err = fmt.Errorf("User function: %v", err)
		return
	}

	defer func() {
		closeErr := rc.Close()
		if closeErr != nil && err == nil {
			err = fmt.Errorf("Close: %v", closeErr)
		}
	}()

	// Copy into the read/write lease.
	copied, err := io.Copy(rwl, rc)
	if err != nil {
		err = fmt.Errorf("Copy: %v", err)
		return
	}

	// Did the user lie about the size?
	if copied != rp.Size() {
		err = fmt.Errorf("Copied %v bytes; expected %v", copied, rp.Size())
		return
	}

	return
}

// Downgrade and save the supplied read/write lease obtained with getContents
// for later use.
func (rp *readProxy) saveContents(rwl ReadWriteLease) {
	rp.lease = rwl.Downgrade()
}

////////////////////////////////////////////////////////////////////////
// Public interface
////////////////////////////////////////////////////////////////////////

func (rp *readProxy) CheckInvariants() {
}

// Semantics matching io.ReaderAt, except with context support.
func (rp *readProxy) ReadAt(
	ctx context.Context,
	p []byte,
	off int64) (n int, err error) {
	// Common case: is the existing lease still valid?
	if rp.lease != nil {
		n, err = rp.lease.ReadAt(p, off)
		if !isRevokedErr(err) {
			return
		}

		// Clear the revoked error.
		err = nil
	}

	// Get hold of a read/write lease containing our contents.
	rwl, err := rp.getContents(ctx)
	if err != nil {
		err = fmt.Errorf("getContents: %v", err)
		return
	}

	defer rp.saveContents(rwl)

	// Serve from the read/write lease.
	n, err = rwl.ReadAt(p, off)

	return
}

// Return the size of the proxied content. Guarantees to not block.
func (rp *readProxy) Size() (size int64) {
	size = rp.size
	return
}

// Return a read/write lease for the proxied contents, destroying the read
// proxy. The read proxy must not be used after calling this method.
func (rp *readProxy) Upgrade(
	ctx context.Context) (rwl ReadWriteLease, err error) {
	// If we succeed, we are now destroyed.
	defer func() {
		if err == nil {
			rp.Destroy()
		}
	}()

	// Common case: is the existing lease still valid?
	if rp.lease != nil {
		rwl, err = rp.lease.Upgrade()
		if !isRevokedErr(err) {
			return
		}

		// Clear the revoked error.
		err = nil
	}

	// Build the read/write lease anew.
	rwl, err = rp.getContents(ctx)
	if err != nil {
		err = fmt.Errorf("getContents: %v", err)
		return
	}

	return
}

// Destroy any resources in use by the read proxy. It must not be used further.
func (rp *readProxy) Destroy() {
	if rp.lease != nil {
		rp.lease.Revoke()
	}

	// Make use-after-destroy errors obvious.
	rp.size = 0
	rp.leaser = nil
	rp.refresher = nil
	rp.lease = nil
}
