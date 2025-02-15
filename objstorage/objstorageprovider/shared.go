// Copyright 2023 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package objstorageprovider

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider/sharedobjcat"
)

// sharedSubsystem contains the provider fields related to shared storage.
// All fields remain unset if shared storage is not configured.
type sharedSubsystem struct {
	catalog *sharedobjcat.Catalog

	// checkRefsOnOpen controls whether we check the ref marker file when opening
	// an object. Normally this is true when invariants are enabled (but the provider
	// test tweaks this field).
	checkRefsOnOpen bool

	// initialized guards access to the creatorID field.
	initialized atomic.Bool
	creatorID   objstorage.CreatorID
	initOnce    sync.Once
}

func (ss *sharedSubsystem) init(creatorID objstorage.CreatorID) {
	ss.initOnce.Do(func() {
		ss.creatorID = creatorID
		ss.initialized.Store(true)
	})
}

// sharedInit initializes the shared object subsystem (if configured) and finds
// any shared objects.
func (p *provider) sharedInit() error {
	if p.st.Shared.Storage == nil {
		return nil
	}
	catalog, contents, err := sharedobjcat.Open(p.st.FS, p.st.FSDirName)
	if err != nil {
		return errors.Wrapf(err, "pebble: could not open shared object catalog")
	}
	p.shared.catalog = catalog
	p.shared.checkRefsOnOpen = invariants.Enabled

	// The creator ID may or may not be initialized yet.
	if contents.CreatorID.IsSet() {
		p.shared.init(contents.CreatorID)
		p.st.Logger.Infof("shared storage configured; creatorID = %s", contents.CreatorID)
	} else {
		p.st.Logger.Infof("shared storage configured; no creatorID yet")
	}

	for _, meta := range contents.Objects {
		o := objstorage.ObjectMetadata{
			FileNum:  meta.FileNum,
			FileType: meta.FileType,
		}
		o.Shared.CreatorID = meta.CreatorID
		o.Shared.CreatorFileNum = meta.CreatorFileNum
		o.Shared.CleanupMethod = meta.CleanupMethod
		p.mu.knownObjects[o.FileNum] = o
	}
	return nil
}

// SetCreatorID is part of the objstorage.Provider interface.
func (p *provider) SetCreatorID(creatorID objstorage.CreatorID) error {
	if p.st.Shared.Storage == nil {
		return errors.AssertionFailedf("attempt to set CreatorID but shared storage not enabled")
	}
	// Note: this call is a cheap no-op if the creator ID was already set. This
	// call also checks if we are trying to change the ID.
	if err := p.shared.catalog.SetCreatorID(creatorID); err != nil {
		return err
	}
	if !p.shared.initialized.Load() {
		p.st.Logger.Infof("shared storage creatorID set to %s", creatorID)
		p.shared.init(creatorID)
	}
	return nil
}

func (p *provider) sharedCheckInitialized() error {
	if p.st.Shared.Storage == nil {
		return errors.Errorf("shared object support not configured")
	}
	if !p.shared.initialized.Load() {
		return errors.Errorf("shared object support not available: shared creator ID not yet set")
	}
	return nil
}

func (p *provider) sharedSync() error {
	batch := func() sharedobjcat.Batch {
		p.mu.Lock()
		defer p.mu.Unlock()
		res := p.mu.shared.catalogBatch.Copy()
		p.mu.shared.catalogBatch.Reset()
		return res
	}()

	if batch.IsEmpty() {
		return nil
	}

	err := p.shared.catalog.ApplyBatch(batch)
	if err != nil {
		// We have to put back the batch (for the next Sync).
		p.mu.Lock()
		defer p.mu.Unlock()
		batch.Append(p.mu.shared.catalogBatch)
		p.mu.shared.catalogBatch = batch
		return err
	}

	return nil
}

func (p *provider) sharedPath(meta objstorage.ObjectMetadata) string {
	return "shared://" + sharedObjectName(meta)
}

// sharedObjectName returns the name of an object on shared storage.
//
// For sstables, the format is: <creator-id>-<file-num>.sst
// For example: 00000000000000000002-000001.sst
func sharedObjectName(meta objstorage.ObjectMetadata) string {
	// TODO(radu): prepend a "shard" value for better distribution within the bucket?
	return fmt.Sprintf(
		"%s-%s",
		meta.Shared.CreatorID, base.MakeFilename(meta.FileType, meta.Shared.CreatorFileNum),
	)
}

// sharedObjectRefName returns the name of the object's ref marker associated
// with this provider. This name is the object's name concatenated with
// ".ref.<provider-id>.<local-file-num>".
//
// For example: 00000000000000000002-000001.sst.ref.00000000000000000005.000008
func (p *provider) sharedObjectRefName(meta objstorage.ObjectMetadata) string {
	if meta.Shared.CleanupMethod != objstorage.SharedRefTracking {
		panic("ref object used when ref tracking disabled")
	}
	return sharedObjectRefName(meta, p.shared.creatorID, meta.FileNum)
}

func sharedObjectRefName(
	meta objstorage.ObjectMetadata, refCreatorID objstorage.CreatorID, refFileNum base.FileNum,
) string {
	if meta.Shared.CleanupMethod != objstorage.SharedRefTracking {
		panic("ref object used when ref tracking disabled")
	}
	return fmt.Sprintf(
		"%s-%s.ref.%s.%s",
		meta.Shared.CreatorID, base.MakeFilename(meta.FileType, meta.Shared.CreatorFileNum), refCreatorID, refFileNum,
	)

}

func sharedObjectRefPrefix(meta objstorage.ObjectMetadata) string {
	return fmt.Sprintf(
		"%s-%s.ref.",
		meta.Shared.CreatorID, base.MakeFilename(meta.FileType, meta.Shared.CreatorFileNum),
	)
}

// sharedCreateRef creates a reference marker object.
func (p *provider) sharedCreateRef(meta objstorage.ObjectMetadata) error {
	if meta.Shared.CleanupMethod != objstorage.SharedRefTracking {
		return nil
	}
	refName := p.sharedObjectRefName(meta)
	writer, err := p.st.Shared.Storage.CreateObject(refName)
	if err == nil {
		// The object is empty, just close the writer.
		err = writer.Close()
	}
	if err != nil {
		return errors.Wrapf(err, "creating marker object %s", refName)
	}
	return nil
}

func (p *provider) sharedCreate(
	_ context.Context, fileType base.FileType, fileNum base.FileNum, opts objstorage.CreateOptions,
) (objstorage.Writable, objstorage.ObjectMetadata, error) {
	if err := p.sharedCheckInitialized(); err != nil {
		return nil, objstorage.ObjectMetadata{}, err
	}
	meta := objstorage.ObjectMetadata{
		FileNum:  fileNum,
		FileType: fileType,
	}
	meta.Shared.CreatorID = p.shared.creatorID
	meta.Shared.CreatorFileNum = fileNum
	meta.Shared.CleanupMethod = opts.SharedCleanupMethod

	objName := sharedObjectName(meta)
	writer, err := p.st.Shared.Storage.CreateObject(objName)
	if err != nil {
		return nil, objstorage.ObjectMetadata{}, errors.Wrapf(err, "creating object %s", objName)
	}
	return &sharedWritable{
		p:             p,
		meta:          meta,
		storageWriter: writer,
	}, meta, nil
}

func (p *provider) sharedOpenForReading(
	ctx context.Context, meta objstorage.ObjectMetadata,
) (objstorage.Readable, error) {
	if err := p.sharedCheckInitialized(); err != nil {
		return nil, err
	}
	// Verify we have a reference on this object; for performance reasons, we only
	// do this in testing scenarios.
	if p.shared.checkRefsOnOpen && meta.Shared.CleanupMethod == objstorage.SharedRefTracking {
		refName := p.sharedObjectRefName(meta)
		if _, err := p.st.Shared.Storage.Size(refName); err != nil {
			// TODO(radu): assertion error if object doesn't exist.
			return nil, errors.Wrapf(err, "checking marker object %s", refName)
		}
	}
	objName := sharedObjectName(meta)
	size, err := p.st.Shared.Storage.Size(objName)
	if err != nil {
		return nil, err
	}
	return newSharedReadable(p.st.Shared.Storage, objName, size), nil
}

func (p *provider) sharedSize(meta objstorage.ObjectMetadata) (int64, error) {
	if err := p.sharedCheckInitialized(); err != nil {
		return 0, err
	}
	objName := sharedObjectName(meta)
	return p.st.Shared.Storage.Size(objName)
}

// sharedUnref implements object "removal" with the shared backend. The ref
// marker object is removed and the backing object is removed only if there are
// no other ref markers.
func (p *provider) sharedUnref(meta objstorage.ObjectMetadata) error {
	if meta.Shared.CleanupMethod == objstorage.SharedNoCleanup {
		// Never delete objects in this mode.
		return nil
	}
	if p.isProtected(meta.FileNum) {
		// TODO(radu): we need a mechanism to unref the object when it becomes
		// unprotected.
		return nil
	}

	refName := p.sharedObjectRefName(meta)
	if err := p.st.Shared.Storage.Delete(refName); err != nil {
		// TODO(radu): continue if it's a not-exist error.
		return err
	}
	otherRefs, err := p.st.Shared.Storage.List(sharedObjectRefPrefix(meta), "" /* delimiter */)
	if err != nil {
		return err
	}
	if len(otherRefs) == 0 {
		objName := sharedObjectName(meta)
		if err := p.st.Shared.Storage.Delete(objName); err != nil {
			// TODO(radu): we must tolerate an object-not-exists here: two providers
			// can race and delete the same object.
			return err
		}
	}
	return err
}
