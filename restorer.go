package lifecycle

import (
	"path/filepath"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/buildpacks/lifecycle/api"
	"github.com/buildpacks/lifecycle/buildpack"
	"github.com/buildpacks/lifecycle/cmd"
	"github.com/buildpacks/lifecycle/layers"
	"github.com/buildpacks/lifecycle/platform"
)

type Restorer struct {
	LayersDir string
	Logger    Logger

	Buildpacks            []buildpack.GroupBuildpack
	LayerMetadataRestorer LayerMetadataRestorer   // Platform API > 0.7
	LayersMetadata        platform.LayersMetadata // Platform API > 0.7
	Platform              cmd.Platform
}

// Restore restores metadata for launch and cache layers into the layers directory and attempts to restore layer data for cache=true layers, removing the layer when unsuccessful.
// If a usable cache is not provided, Restore will not restore any cache=true layer metadata.
func (r *Restorer) Restore(cache Cache) error {
	cacheMeta, err := retrieveCacheMetadata(cache, r.Logger)
	if err != nil {
		return err
	}

	if r.restoresLayerMetadata() {
		if err := r.LayerMetadataRestorer.Restore(r.Buildpacks, r.LayersMetadata, cacheMeta); err != nil {
			return err
		}
	}

	var g errgroup.Group
	for _, buildpack := range r.Buildpacks {
		cachedLayers := cacheMeta.MetadataForBuildpack(buildpack.ID).Layers

		var cachedFn func(bpLayer) bool
		if api.MustParse(buildpack.API).Compare(api.MustParse("0.6")) >= 0 {
			// On Buildpack API 0.6+, the <layer>.toml file never contains layer types information.
			// The cache metadata is the only way to identify cache=true layers.
			cachedFn = func(l bpLayer) bool {
				layer, ok := cachedLayers[filepath.Base(l.path)]
				return ok && layer.Cache
			}
		} else {
			// On Buildpack API < 0.6, the <layer>.toml file contains layer types information.
			// Prefer <layer>.toml file to cache metadata in case the cache was cleared between builds and
			// the analyzer that wrote the files is on a previous version of the lifecycle, that doesn't cross-reference the cache metadata when writing the files.
			// This allows the restorer to cleanup <layer>.toml files for layers that are not actually in the cache.
			cachedFn = forCached
		}

		buildpackDir, err := readBuildpackLayersDir(r.LayersDir, buildpack, r.Logger)
		if err != nil {
			return errors.Wrapf(err, "reading buildpack layer directory")
		}
		foundLayers := buildpackDir.findLayers(cachedFn)

		for _, bpLayer := range foundLayers {
			name := bpLayer.name()
			cachedLayer, exists := cachedLayers[name]
			if !exists {
				r.Logger.Infof("Removing %q, not in cache", bpLayer.Identifier())
				if err := bpLayer.remove(); err != nil {
					return errors.Wrapf(err, "removing layer")
				}
				continue
			}
			data, err := bpLayer.read()
			if err != nil {
				return errors.Wrapf(err, "reading layer")
			}
			if data.SHA != cachedLayer.SHA {
				r.Logger.Infof("Removing %q, wrong sha", bpLayer.Identifier())
				r.Logger.Debugf("Layer sha: %q, cache sha: %q", data.SHA, cachedLayer.SHA)
				if err := bpLayer.remove(); err != nil {
					return errors.Wrapf(err, "removing layer")
				}
			} else {
				r.Logger.Infof("Restoring data for %q from cache", bpLayer.Identifier())
				g.Go(func() error {
					return r.restoreLayer(cache, cachedLayer.SHA)
				})
			}
		}
	}
	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "restoring data")
	}
	return nil
}

func (r *Restorer) restoresLayerMetadata() bool {
	return api.MustParse(r.Platform.API()).Compare(api.MustParse("0.7")) >= 0
}

func (r *Restorer) restoreLayer(cache Cache, sha string) error {
	// Sanity check to prevent panic.
	if cache == nil {
		return errors.New("restoring layer: cache not provided")
	}
	r.Logger.Debugf("Retrieving data for %q", sha)
	rc, err := cache.RetrieveLayer(sha)
	if err != nil {
		return err
	}
	defer rc.Close()

	return layers.Extract(rc, "")
}
