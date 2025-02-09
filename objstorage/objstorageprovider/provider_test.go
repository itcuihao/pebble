// Copyright 2023 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package objstorageprovider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/shared"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func TestProvider(t *testing.T) {
	datadriven.Walk(t, "testdata/provider", func(t *testing.T, path string) {
		var log base.InMemLogger
		fs := vfs.WithLogging(vfs.NewMem(), func(fmt string, args ...interface{}) {
			log.Infof("<local fs> "+fmt, args...)
		})
		sharedStore := shared.WithLogging(shared.NewInMem(), func(fmt string, args ...interface{}) {
			log.Infof("<shared> "+fmt, args...)
		})

		providers := make(map[string]objstorage.Provider)
		// We maintain both backings and backing handles to allow tests to use the
		// backings after the handles have been closed.
		backings := make(map[string]objstorage.SharedObjectBacking)
		backingHandles := make(map[string]objstorage.SharedObjectBackingHandle)
		var curProvider objstorage.Provider
		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			scanArgs := func(desc string, args ...interface{}) {
				t.Helper()
				if len(d.CmdArgs) != len(args) {
					d.Fatalf(t, "usage: %s %s", d.Cmd, desc)
				}
				for i := range args {
					_, err := fmt.Sscan(d.CmdArgs[i].String(), args[i])
					if err != nil {
						d.Fatalf(t, "%s: error parsing argument '%s'", d.Cmd, d.CmdArgs[i])
					}
				}
			}
			ctx := context.Background()

			log.Reset()
			switch d.Cmd {
			case "open":
				var fsDir string
				var creatorID objstorage.CreatorID
				scanArgs("<fs-dir> <shared-creator-id>", &fsDir, &creatorID)

				st := DefaultSettings(fs, fsDir)
				if creatorID != 0 {
					st.Shared.Storage = sharedStore
				}
				require.NoError(t, fs.MkdirAll(fsDir, 0755))
				p, err := Open(st)
				require.NoError(t, err)
				if creatorID != 0 {
					require.NoError(t, p.SetCreatorID(creatorID))
				}
				// Checking refs on open affects the test output. We don't want tests to
				// only pass when the `invariants` tag is used, so unconditionally
				// enable ref checking on open.
				p.(*provider).shared.checkRefsOnOpen = true
				providers[fsDir] = p
				curProvider = p

				return log.String()

			case "switch":
				var fsDir string
				scanArgs("<fs-dir>", &fsDir)
				curProvider = providers[fsDir]
				if curProvider == nil {
					t.Fatalf("unknown provider %s", fsDir)
				}

				return ""

			case "close":
				require.NoError(t, curProvider.Sync())
				require.NoError(t, curProvider.Close())
				delete(providers, curProvider.(*provider).st.FSDirName)
				curProvider = nil

				return log.String()

			case "create":
				opts := objstorage.CreateOptions{
					SharedCleanupMethod: objstorage.SharedRefTracking,
				}
				if len(d.CmdArgs) == 3 && d.CmdArgs[2].Key == "no-ref-tracking" {
					d.CmdArgs = d.CmdArgs[:2]
					opts.SharedCleanupMethod = objstorage.SharedNoCleanup
				}
				var fileNum base.FileNum
				var typ string
				scanArgs("<file-num> <local|shared> [no-ref-tracking]", &fileNum, &typ)
				switch typ {
				case "local":
				case "shared":
					opts.PreferSharedStorage = true
				default:
					d.Fatalf(t, "'%s' should be 'local' or 'shared'", typ)
				}
				w, _, err := curProvider.Create(ctx, base.FileTypeTable, fileNum, opts)
				if err != nil {
					return err.Error()
				}
				require.NoError(t, w.Write([]byte(d.Input)))
				require.NoError(t, w.Finish())

				return log.String()

			case "read":
				var fileNum base.FileNum
				scanArgs("<file-num>", &fileNum)
				r, err := curProvider.OpenForReading(ctx, base.FileTypeTable, fileNum, objstorage.OpenOptions{})
				if err != nil {
					return err.Error()
				}
				data := make([]byte, int(r.Size()))
				n, err := r.ReadAt(ctx, data, 0)
				require.NoError(t, err)
				require.Equal(t, n, len(data))
				return log.String() + fmt.Sprintf("data: %s\n", string(data))

			case "remove":
				var fileNum base.FileNum
				scanArgs("<file-num>", &fileNum)
				if err := curProvider.Remove(base.FileTypeTable, fileNum); err != nil {
					return err.Error()
				}
				return log.String()

			case "list":
				for _, meta := range curProvider.List() {
					log.Infof("%s -> %s", meta.FileNum, curProvider.Path(meta))
				}
				return log.String()

			case "save-backing":
				var key string
				var fileNum base.FileNum
				scanArgs("<key> <file-num>", &key, &fileNum)
				meta, err := curProvider.Lookup(base.FileTypeTable, fileNum)
				require.NoError(t, err)
				handle, err := curProvider.SharedObjectBacking(&meta)
				if err != nil {
					return err.Error()
				}
				backing, err := handle.Get()
				require.NoError(t, err)
				backings[key] = backing
				backingHandles[key] = handle
				return log.String()

			case "close-backing":
				var key string
				scanArgs("<key>", &key)
				backingHandles[key].Close()
				return ""

			case "attach":
				lines := strings.Split(d.Input, "\n")
				if len(lines) == 0 {
					d.Fatalf(t, "at least one row expected; format: <key> <file-num>")
				}
				var objs []objstorage.SharedObjectToAttach
				for _, l := range lines {
					var key string
					var fileNum base.FileNum
					_, err := fmt.Sscan(l, &key, &fileNum)
					require.NoError(t, err)
					b, ok := backings[key]
					if !ok {
						d.Fatalf(t, "unknown backing key %q", key)
					}
					objs = append(objs, objstorage.SharedObjectToAttach{
						FileType: base.FileTypeTable,
						FileNum:  fileNum,
						Backing:  b,
					})
				}
				metas, err := curProvider.AttachSharedObjects(objs)
				if err != nil {
					return log.String() + "error: " + err.Error()
				}
				for _, meta := range metas {
					log.Infof("%s -> %s", meta.FileNum, curProvider.Path(meta))
				}
				return log.String()

			default:
				d.Fatalf(t, "unknown command %s", d.Cmd)
				return ""
			}
		})
	})
}

func TestNotExistError(t *testing.T) {
	// TODO(radu): test with shared objects.
	var log base.InMemLogger
	fs := vfs.WithLogging(vfs.NewMem(), log.Infof)
	provider, err := Open(DefaultSettings(fs, ""))
	require.NoError(t, err)

	require.True(t, provider.IsNotExistError(provider.Remove(base.FileTypeTable, 1)))
	_, err = provider.OpenForReading(context.Background(), base.FileTypeTable, 1, objstorage.OpenOptions{})
	require.True(t, provider.IsNotExistError(err))

	w, _, err := provider.Create(context.Background(), base.FileTypeTable, 1, objstorage.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, w.Write([]byte("foo")))
	require.NoError(t, w.Finish())

	// Remove the underlying file.
	require.NoError(t, fs.Remove(base.MakeFilename(base.FileTypeTable, 1)))
	require.True(t, provider.IsNotExistError(provider.Remove(base.FileTypeTable, 1)))
}
