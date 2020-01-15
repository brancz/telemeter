// Package block contains common functionality for interacting with TSDB blocks
// in the context of Thanos.
package block

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-kit/kit/log/level"

	"github.com/thanos-io/thanos/pkg/block/metadata"

	"fmt"

	"github.com/go-kit/kit/log"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/runutil"
)

const (
	// MetaFilename is the known JSON filename for meta information.
	MetaFilename = "meta.json"
	// IndexFilename is the known index file for block index.
	IndexFilename = "index"
	// IndexCacheFilename is the canonical name for index cache file that stores essential information needed.
	IndexCacheFilename = "index.cache.json"
	// ChunksDirname is the known dir name for chunks with compressed samples.
	ChunksDirname = "chunks"

	// DebugMetas is a directory for debug meta files that happen in the past. Useful for debugging.
	DebugMetas = "debug/metas"
)

// Download downloads directory that is mean to be block directory.
func Download(ctx context.Context, logger log.Logger, bucket objstore.Bucket, id ulid.ULID, dst string) error {
	if err := objstore.DownloadDir(ctx, logger, bucket, id.String(), dst); err != nil {
		return err
	}

	chunksDir := filepath.Join(dst, ChunksDirname)
	_, err := os.Stat(chunksDir)
	if os.IsNotExist(err) {
		// This can happen if block is empty. We cannot easily upload empty directory, so create one here.
		return os.Mkdir(chunksDir, os.ModePerm)
	}

	if err != nil {
		return errors.Wrapf(err, "stat %s", chunksDir)
	}

	return nil
}

// Upload uploads block from given block dir that ends with block id.
// It makes sure cleanup is done on error to avoid partial block uploads.
// It also verifies basic features of Thanos block.
// TODO(bplotka): Ensure bucket operations have reasonable backoff retries.
func Upload(ctx context.Context, logger log.Logger, bkt objstore.Bucket, bdir string) error {
	df, err := os.Stat(bdir)
	if err != nil {
		return err
	}
	if !df.IsDir() {
		return errors.Errorf("%s is not a directory", bdir)
	}

	// Verify dir.
	id, err := ulid.Parse(df.Name())
	if err != nil {
		return errors.Wrap(err, "not a block dir")
	}

	meta, err := metadata.Read(bdir)
	if err != nil {
		// No meta or broken meta file.
		return errors.Wrap(err, "read meta")
	}

	if meta.Thanos.Labels == nil || len(meta.Thanos.Labels) == 0 {
		return errors.Errorf("empty external labels are not allowed for Thanos block.")
	}

	if err := objstore.UploadFile(ctx, logger, bkt, path.Join(bdir, MetaFilename), path.Join(DebugMetas, fmt.Sprintf("%s.json", id))); err != nil {
		return errors.Wrap(err, "upload meta file to debug dir")
	}

	if err := objstore.UploadDir(ctx, logger, bkt, path.Join(bdir, ChunksDirname), path.Join(id.String(), ChunksDirname)); err != nil {
		return cleanUp(logger, bkt, id, errors.Wrap(err, "upload chunks"))
	}

	if err := objstore.UploadFile(ctx, logger, bkt, path.Join(bdir, IndexFilename), path.Join(id.String(), IndexFilename)); err != nil {
		return cleanUp(logger, bkt, id, errors.Wrap(err, "upload index"))
	}

	if meta.Thanos.Source == metadata.CompactorSource {
		if err := objstore.UploadFile(ctx, logger, bkt, path.Join(bdir, IndexCacheFilename), path.Join(id.String(), IndexCacheFilename)); err != nil {
			return cleanUp(logger, bkt, id, errors.Wrap(err, "upload index cache"))
		}
	}

	// Meta.json always need to be uploaded as a last item. This will allow to assume block directories without meta file
	// to be pending uploads.
	if err := objstore.UploadFile(ctx, logger, bkt, path.Join(bdir, MetaFilename), path.Join(id.String(), MetaFilename)); err != nil {
		return cleanUp(logger, bkt, id, errors.Wrap(err, "upload meta file"))
	}

	return nil
}

func cleanUp(logger log.Logger, bkt objstore.Bucket, id ulid.ULID, err error) error {
	// Cleanup the dir with an uncancelable context.
	cleanErr := Delete(context.Background(), logger, bkt, id)
	if cleanErr != nil {
		return errors.Wrapf(err, "failed to clean block after upload issue. Partial block in system. Err: %s", err.Error())
	}
	return err
}

// Delete removes directory that is meant to be block directory.
// NOTE: Always prefer this method for deleting blocks.
//  * We have to delete block's files in the certain order (meta.json first)
//  to ensure we don't end up with malformed partial blocks. Thanos system handles well partial blocks
//  only if they don't have meta.json. If meta.json is present Thanos assumes valid block.
//  * This avoids deleting empty dir (whole bucket) by mistake.
func Delete(ctx context.Context, logger log.Logger, bkt objstore.Bucket, id ulid.ULID) error {
	metaFile := path.Join(id.String(), MetaFilename)
	ok, err := bkt.Exists(ctx, metaFile)
	if err != nil {
		return errors.Wrapf(err, "stat %s", metaFile)
	}
	if ok {
		if err := bkt.Delete(ctx, metaFile); err != nil {
			return errors.Wrapf(err, "delete %s", metaFile)
		}
		level.Debug(logger).Log("msg", "deleted file", "file", metaFile, "bucket", bkt.Name())
	}

	return deleteDir(ctx, logger, bkt, id.String())
}

// deleteDir removes all objects prefixed with dir from the bucket.
// NOTE: For objects removal use `block.Delete` strictly.
func deleteDir(ctx context.Context, logger log.Logger, bkt objstore.Bucket, dir string) error {
	return bkt.Iter(ctx, dir, func(name string) error {
		// If we hit a directory, call DeleteDir recursively.
		if strings.HasSuffix(name, objstore.DirDelim) {
			return deleteDir(ctx, logger, bkt, name)
		}
		if err := bkt.Delete(ctx, name); err != nil {
			return err
		}
		level.Debug(logger).Log("msg", "deleted file", "file", name, "bucket", bkt.Name())
		return nil
	})
}

// DownloadMeta downloads only meta file from bucket by block ID.
// TODO(bwplotka): Differentiate between network error & partial upload.
func DownloadMeta(ctx context.Context, logger log.Logger, bkt objstore.Bucket, id ulid.ULID) (metadata.Meta, error) {
	rc, err := bkt.Get(ctx, path.Join(id.String(), MetaFilename))
	if err != nil {
		return metadata.Meta{}, errors.Wrapf(err, "meta.json bkt get for %s", id.String())
	}
	defer runutil.CloseWithLogOnErr(logger, rc, "download meta bucket client")

	var m metadata.Meta

	obj, err := ioutil.ReadAll(rc)
	if err != nil {
		return metadata.Meta{}, errors.Wrapf(err, "read meta.json for block %s", id.String())
	}

	if err = json.Unmarshal(obj, &m); err != nil {
		return metadata.Meta{}, errors.Wrapf(err, "unmarshal meta.json for block %s", id.String())
	}

	return m, nil
}

func IsBlockDir(path string) (id ulid.ULID, ok bool) {
	id, err := ulid.Parse(filepath.Base(path))
	return id, err == nil
}