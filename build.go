package bundler

import (
	"path/filepath"
	"time"

	"github.com/Masterminds/semver"
	"github.com/paketo-buildpacks/packit"
	"github.com/paketo-buildpacks/packit/chronos"
	"github.com/paketo-buildpacks/packit/postal"
)

//go:generate faux --interface EntryResolver --output fakes/entry_resolver.go
type EntryResolver interface {
	Resolve(string, []packit.BuildpackPlanEntry, []interface{}) (packit.BuildpackPlanEntry, []packit.BuildpackPlanEntry)
	MergeLayerTypes(string, []packit.BuildpackPlanEntry) (launch, build bool)
}

//go:generate faux --interface DependencyManager --output fakes/dependency_manager.go
type DependencyManager interface {
	Resolve(path, id, version, stack string) (postal.Dependency, error)
	Install(dependency postal.Dependency, cnbPath, layerPath string) error
}

//go:generate faux --interface BuildPlanRefinery --output fakes/build_plan_refinery.go
type BuildPlanRefinery interface {
	BillOfMaterial(dependency postal.Dependency) packit.BuildpackPlan
}

//go:generate faux --interface Shimmer --output fakes/shimmer.go
type Shimmer interface {
	Shim(path, version string) error
}

func Build(
	entries EntryResolver,
	dependencies DependencyManager,
	planRefinery BuildPlanRefinery,
	logger LogEmitter,
	clock chronos.Clock,
	versionShimmer Shimmer,
) packit.BuildFunc {
	return func(context packit.BuildContext) (packit.BuildResult, error) {
		logger.Title("%s %s", context.BuildpackInfo.Name, context.BuildpackInfo.Version)
		logger.Process("Resolving Bundler version")

		entry, allEntries := entries.Resolve("bundler", context.Plan.Entries, []interface{}{"BP_BUNDLER_VERSION", "buildpack.yml", "Gemfile.lock"})
		logger.Candidates(allEntries)

		version, _ := entry.Metadata["version"].(string)
		dependency, err := dependencies.Resolve(filepath.Join(context.CNBPath, "buildpack.toml"), entry.Name, version, context.Stack)
		if err != nil {
			return packit.BuildResult{}, err
		}

		logger.SelectedDependency(entry, dependency, clock.Now())

		source, _ := entry.Metadata["version-source"].(string)
		if source == "buildpack.yml" {
			nextMajorVersion := semver.MustParse(context.BuildpackInfo.Version).IncMajor()
			logger.Subprocess("WARNING: Setting the Bundler version through buildpack.yml will be deprecated soon in Bundler Buildpack v%s.", nextMajorVersion.String())
			logger.Subprocess("Please specify the version through the $BP_BUNDLER_VERSION environment variable instead. See README.md for more information.")
			logger.Break()
		}

		bundlerLayer, err := context.Layers.Get(Bundler)
		if err != nil {
			return packit.BuildResult{}, err
		}

		bom := planRefinery.BillOfMaterial(postal.Dependency{
			ID:      dependency.ID,
			Name:    dependency.Name,
			SHA256:  dependency.SHA256,
			Stacks:  dependency.Stacks,
			URI:     dependency.URI,
			Version: dependency.Version,
		})

		cachedSHA, ok := bundlerLayer.Metadata[DepKey].(string)
		if ok && cachedSHA == dependency.SHA256 {
			logger.Process("Reusing cached layer %s", bundlerLayer.Path)
			logger.Break()

			return packit.BuildResult{
				Plan:   bom,
				Layers: []packit.Layer{bundlerLayer},
			}, nil
		}

		logger.Process("Executing build process")

		bundlerLayer, err = bundlerLayer.Reset()
		if err != nil {
			return packit.BuildResult{}, err
		}

		bundlerLayer.Launch, bundlerLayer.Build = entries.MergeLayerTypes("bundler", context.Plan.Entries)
		bundlerLayer.Cache = bundlerLayer.Build

		logger.Subprocess("Installing Bundler %s", dependency.Version)
		duration, err := clock.Measure(func() error {
			err := dependencies.Install(dependency, context.CNBPath, bundlerLayer.Path)
			if err != nil {
				return err
			}

			return versionShimmer.Shim(filepath.Join(bundlerLayer.Path, "bin"), dependency.Version)
		})
		if err != nil {
			return packit.BuildResult{}, err
		}

		logger.Action("Completed in %s", duration.Round(time.Millisecond))
		logger.Break()

		bundlerLayer.Metadata = map[string]interface{}{
			DepKey:     dependency.SHA256,
			"built_at": clock.Now().Format(time.RFC3339Nano),
		}

		bundlerLayer.SharedEnv.Append("GEM_PATH", bundlerLayer.Path, ":")

		logger.Environment(bundlerLayer.SharedEnv)

		return packit.BuildResult{
			Plan:   bom,
			Layers: []packit.Layer{bundlerLayer},
		}, nil
	}
}
