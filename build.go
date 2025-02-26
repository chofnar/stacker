package stacker

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"path"
	"runtime"
	"strings"
	"time"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/mutate"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/pkg/errors"
	"github.com/project-stacker/stacker/container"
	"github.com/project-stacker/stacker/log"
	"github.com/project-stacker/stacker/types"
)

const DefaultShell = "/usr/bin/sh"

type BuildArgs struct {
	Config       types.StackerConfig
	LeaveUnladen bool
	NoCache      bool
	Substitute   []string
	OnRunFailure string
	LayerTypes   []types.LayerType
	OrderOnly    bool
	HashRequired bool
	SetupOnly    bool
	Progress     bool
}

// Builder is responsible for building the layers based on stackerfiles
type Builder struct {
	builtStackerfiles types.StackerFiles // Keep track of all the Stackerfiles which were built
	opts              *BuildArgs         // Build options
}

// NewBuilder initializes a new Builder struct
func NewBuilder(opts *BuildArgs) *Builder {
	return &Builder{
		builtStackerfiles: make(map[string]*types.Stackerfile, 1),
		opts:              opts,
	}
}

func (b *Builder) updateOCIConfigForOutput(sf *types.Stackerfile, s types.Storage, oci casext.Engine, layerType types.LayerType, l types.Layer, name string) error {
	opts := b.opts

	layerName := layerType.LayerName(name)
	descPaths, err := oci.ResolveReference(context.Background(), layerName)
	if err != nil {
		return err
	}

	mutator, err := mutate.New(oci, descPaths[0])
	if err != nil {
		return errors.Wrapf(err, "mutator failed")
	}

	config, err := mutator.Config(context.Background())
	if err != nil {
		return err
	}

	imageConfig := config.Config

	if imageConfig.Labels == nil {
		imageConfig.Labels = map[string]string{}
	}

	if len(l.GenerateLabels) > 0 {
		writable, cleanup, err := s.TemporaryWritableSnapshot(name)
		if err != nil {
			return err
		}
		defer cleanup()

		dir, err := ioutil.TempDir(opts.Config.StackerDir, fmt.Sprintf("oci-labels-%s-", name))
		if err != nil {
			return errors.Wrapf(err, "failed to create oci-labels tempdir")
		}
		defer os.RemoveAll(dir)

		c, err := container.New(opts.Config, writable)
		if err != nil {
			return err
		}
		defer c.Close()

		err = SetupBuildContainerConfig(opts.Config, s, c, writable)
		if err != nil {
			return err
		}

		err = c.BindMount(dir, "/oci-labels", "")
		if err != nil {
			return err
		}

		rootfs := path.Join(opts.Config.RootFSDir, writable, "rootfs")
		runPath := path.Join(dir, ".stacker-run.sh")
		err = generateShellForRunning(rootfs, l.GenerateLabels, runPath)
		if err != nil {
			return err
		}

		err = c.Execute("/oci-labels/.stacker-run.sh", nil)
		if err != nil {
			return err
		}

		ents, err := ioutil.ReadDir(dir)
		if err != nil {
			return errors.Wrapf(err, "failed to read %s", dir)
		}

		for _, ent := range ents {
			if ent.Name() == ".stacker-run.sh" {
				continue
			}

			content, err := ioutil.ReadFile(path.Join(dir, ent.Name()))
			if err != nil {
				return errors.Wrapf(err, "couldn't read label %s", ent.Name())
			}

			imageConfig.Labels[ent.Name()] = string(content)
		}
	}

	pathSet := false
	for k, v := range l.Environment {
		if k == "PATH" {
			pathSet = true
		}
		imageConfig.Env = append(imageConfig.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if !pathSet {
		for _, s := range imageConfig.Env {
			if strings.HasPrefix(s, "PATH=") {
				pathSet = true
				break
			}
		}
	}

	// if the user didn't specify a path, let's set a sane one
	if !pathSet {
		imageConfig.Env = append(imageConfig.Env, fmt.Sprintf("PATH=%s", container.ReasonableDefaultPath))
	}

	imageConfig.Cmd = l.Cmd
	imageConfig.Entrypoint = l.Entrypoint
	if l.FullCommand != nil {
		imageConfig.Cmd = nil
		imageConfig.Entrypoint = l.FullCommand
	}

	if imageConfig.Volumes == nil {
		imageConfig.Volumes = map[string]struct{}{}
	}

	for _, v := range l.Volumes {
		imageConfig.Volumes[v] = struct{}{}
	}

	for k, v := range l.Labels {
		imageConfig.Labels[k] = v
	}

	if l.WorkingDir != "" {
		imageConfig.WorkingDir = l.WorkingDir
	}

	if l.RuntimeUser != "" {
		imageConfig.User = l.RuntimeUser
	}

	meta, err := mutator.Meta(context.Background())
	if err != nil {
		return err
	}

	username := os.Getenv("SUDO_USER")

	if username == "" {
		user, err := user.Current()
		if err != nil {
			return err
		}

		username = user.Username
	}

	host, err := os.Hostname()
	if err != nil {
		return err
	}

	author := fmt.Sprintf("%s@%s", username, host)

	meta.Created = time.Now()
	meta.Architecture = runtime.GOARCH
	meta.OS = runtime.GOOS
	meta.Author = author

	annotations, err := mutator.Annotations(context.Background())
	if err != nil {
		return err
	}

	// compute the git version for the directory that the stacker file is
	// in. we don't care if it's not a git directory, because in that case
	// we'll fall back to putting the whole stacker file contents in the
	// metadata.
	gitVersion, _ := GitVersion(sf.ReferenceDirectory)

	if gitVersion != "" {
		log.Debugf("setting git version annotation to %s", gitVersion)
		annotations[GitVersionAnnotation] = gitVersion
	}

	annotations[StackerContentsAnnotation] = sf.AfterSubstitutions

	history := ispec.History{
		EmptyLayer: true, // this is only the history for imageConfig edit
		Created:    &meta.Created,
		CreatedBy:  "stacker build",
		Author:     author,
	}

	err = mutator.Set(context.Background(), imageConfig, meta, annotations, &history)
	if err != nil {
		return err
	}

	newPath, err := mutator.Commit(context.Background())
	if err != nil {
		return err
	}

	err = oci.UpdateReference(context.Background(), layerName, newPath.Root())
	if err != nil {
		return err
	}

	return nil
}

// Build builds a single stackerfile
func (b *Builder) build(s types.Storage, file string) error {
	opts := b.opts

	if opts.NoCache {
		os.RemoveAll(opts.Config.StackerDir)
	}

	sf, err := types.NewStackerfile(file, opts.HashRequired, append(opts.Substitute, b.opts.Config.Substitutions()...))
	if err != nil {
		return err
	}

	order, err := sf.DependencyOrder(b.builtStackerfiles)
	if err != nil {
		return err
	}

	/* check that layers name don't contain ':', it will interfere with overlay mount options
	which is using :s as separator */
	for _, name := range order {
		if strings.Contains(name, ":") {
			return errors.Errorf("using ':' in the layer name (%s) is forbidden due to overlay constraints", name)
		}
	}

	log.Debugf("Dependency Order %v", order)

	var oci casext.Engine
	if _, statErr := os.Stat(opts.Config.OCIDir); statErr != nil {
		oci, err = umoci.CreateLayout(opts.Config.OCIDir)
	} else {
		oci, err = umoci.OpenLayout(opts.Config.OCIDir)
	}
	if err != nil {
		return err
	}
	defer oci.Close()

	// Add this stackerfile to the list of stackerfiles which were built
	b.builtStackerfiles[file] = sf
	buildCache, err := OpenCache(opts.Config, oci, b.builtStackerfiles)
	if err != nil {
		return err
	}

	for _, name := range order {
		l, ok := sf.Get(name)
		if !ok {
			return errors.Errorf("%s not present in stackerfile?", name)
		}

		// if a container builds on another container in a stacker
		// file, we can't correctly render the dependent container's
		// filesystem, since we don't know what the output of the
		// parent build will be. so let's refuse to run in setup-only
		// mode in this case.
		if opts.SetupOnly && l.From.Type == types.BuiltLayer {
			return errors.Errorf("no built type layers (%s) allowed in setup mode", name)
		}

		log.Infof("preparing image %s...", name)

		// We need to run the imports first since we now compare
		// against imports for caching layers. Since we don't do
		// network copies if the files are present and we use rsync to
		// copy things across, hopefully this isn't too expensive.
		err = CleanImportsDir(opts.Config, name, l.Imports, buildCache)
		if err != nil {
			return err
		}

		if err := Import(opts.Config, s, name, l.Imports, opts.Progress); err != nil {
			return err
		}

		// Need to check if the image has bind mounts, if the image has bind mounts,
		// it needs to be rebuilt regardless of the build cache
		// The reason is that tracking build cache for bind mounted folders
		// is too expensive, so we don't do it
		baseOpts := BaseLayerOpts{
			Config:     opts.Config,
			Name:       name,
			Layer:      l,
			Cache:      buildCache,
			OCI:        oci,
			LayerTypes: opts.LayerTypes,
			Storage:    s,
			Progress:   opts.Progress,
		}

		if err := GetBase(baseOpts); err != nil {
			return err
		}

		cacheEntry, cacheHit, err := buildCache.Lookup(name)
		if err != nil {
			return err
		}
		if cacheHit && (len(l.Binds) == 0) {
			if l.BuildOnly {
				if cacheEntry.Name != name {
					err = s.Snapshot(cacheEntry.Name, name)
					if err != nil {
						return err
					}
				}
				continue
			} else {
				foundCount := 0
				for _, layerType := range opts.LayerTypes {
					blob, ok := cacheEntry.Manifests[layerType]
					if ok {
						foundCount += 1
						layerName := layerType.LayerName(name)
						err = oci.UpdateReference(context.Background(), layerName, blob)
						if err != nil {
							return err
						}
						log.Infof("found cached layer %s", layerName)
					}
				}

				if foundCount == len(opts.LayerTypes) {
					continue
				}

				log.Infof("missing some cached layer output types, building anyway")
			}
		} else if cacheHit && (len(l.Binds) > 0) {
			log.Infof("rebuilding cached layer due to use of binds in stacker file")
		}

		err = SetupRootfs(baseOpts)
		if err != nil {
			return err
		}

		err = s.SetOverlayDirs(name, l.OverlayDirs, opts.LayerTypes)
		if err != nil {
			return err
		}

		c, err := container.New(opts.Config, name)
		if err != nil {
			return err
		}
		defer c.Close()

		err = SetupBuildContainerConfig(opts.Config, s, c, name)
		if err != nil {
			return err
		}

		err = SetupLayerConfig(opts.Config, c, l, name)
		if err != nil {
			return err
		}

		if opts.SetupOnly {
			err = c.SaveConfigFile(path.Join(opts.Config.RootFSDir, name, "lxc.conf"))
			if err != nil {
				return errors.Wrapf(err, "error saving config file for %s", name)
			}

			log.Infof("setup for %s complete", name)
			continue
		}

		if len(l.Run) != 0 {
			rootfs := path.Join(opts.Config.RootFSDir, name, "rootfs")
			shellScript := path.Join(opts.Config.StackerDir, "imports", name, ".stacker-run.sh")
			err = generateShellForRunning(rootfs, l.Run, shellScript)
			if err != nil {
				return err
			}

			// These should all be non-interactive; let's ensure that.
			err = c.Execute("/stacker/.stacker-run.sh", nil)
			if err != nil {
				if opts.OnRunFailure != "" {
					err2 := c.Execute(opts.OnRunFailure, os.Stdin)
					if err2 != nil {
						log.Infof("failed executing %s: %s\n", opts.OnRunFailure, err2)
					}
				}
				return errors.Errorf("run commands failed: %s", err)
			}
		}

		// This is a build only layer, meaning we don't need to include
		// it in the final image, as outputs from it are going to be
		// imported into future images. Let's just snapshot it and add
		// a bogus entry to our cache.
		if l.BuildOnly {
			log.Debugf("build only layer, skipping OCI diff generation")

			// A small hack: for build only layers, we keep track
			// of the name, so we can make sure it exists when
			// there is a cache hit. We should probably make this
			// into some sort of proper Either type.
			manifests := map[types.LayerType]ispec.Descriptor{opts.LayerTypes[0]: ispec.Descriptor{}}
			if err := buildCache.Put(name, manifests); err != nil {
				return err
			}
			continue
		}

		err = s.Repack(name, opts.LayerTypes, b.builtStackerfiles)
		if err != nil {
			return err
		}

		manifests := map[types.LayerType]ispec.Descriptor{}
		for _, layerType := range opts.LayerTypes {
			err = b.updateOCIConfigForOutput(sf, s, oci, layerType, l, name)
			if err != nil {
				return err
			}

			descPaths, err := oci.ResolveReference(context.Background(), layerType.LayerName(name))
			if err != nil {
				return err
			}

			manifests[layerType] = descPaths[0].Descriptor()

		}

		if err := buildCache.Put(name, manifests); err != nil {
			return err
		}

		log.Infof("filesystem %s built successfully", name)

	}

	return oci.GC(context.Background())
}

// BuildMultiple builds a list of stackerfiles
func (b *Builder) BuildMultiple(paths []string) error {
	opts := b.opts

	s, locks, err := NewStorage(opts.Config)
	if err != nil {
		return err
	}
	defer locks.Unlock()

	// Read all the stacker recipes
	stackerFiles, err := types.NewStackerFiles(paths, opts.HashRequired, append(opts.Substitute, b.opts.Config.Substitutions()...))
	if err != nil {
		return err
	}

	// Initialize the DAG
	dag, err := NewStackerFilesDAG(stackerFiles)
	if err != nil {
		return err
	}

	sortedPaths := dag.Sort()

	// Show the serial build order
	log.Debugf("stacker build order:")
	for i, p := range sortedPaths {
		prerequisites, err := dag.GetStackerFile(p).Prerequisites()
		if err != nil {
			return err
		}
		log.Debugf("%d build %s: requires: %v", i, p, prerequisites)
	}

	if opts.OrderOnly {
		// User has requested only to see the build order, so skipping the actual build
		return nil
	}

	// Build all Stackerfiles
	for i, p := range sortedPaths {
		log.Debugf("building: %d %s", i, p)

		err = b.build(s, p)
		if err != nil {
			return err
		}
	}

	return nil
}

// generateShellForRunning generates a shell script to run inside the
// container, and writes it to the contianer. It checks that the script already
// have a shebang? If so, it leaves it as is, otherwise it prepends a shebang.
func generateShellForRunning(rootfs string, cmd []string, outFile string) error {
	shebangLine := fmt.Sprintf("#!%s -xe\n", DefaultShell)
	if strings.HasPrefix(cmd[0], "#!") {
		shebangLine = ""
	}
	return ioutil.WriteFile(outFile, []byte(shebangLine+strings.Join(cmd, "\n")+"\n"), 0755)
}

func runInternalGoSubcommand(config types.StackerConfig, args []string) error {
	binary, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return err
	}

	cmd := []string{
		"--oci-dir", config.OCIDir,
		"--roots-dir", config.RootFSDir,
		"--stacker-dir", config.StackerDir,
		"--storage-type", config.StorageType,
		"--internal-userns",
	}

	if config.Debug {
		cmd = append(cmd, "--debug")
	}

	cmd = append(cmd, "internal-go")
	cmd = append(cmd, args...)
	c := exec.Command(binary, cmd...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	return errors.WithStack(c.Run())
}

func SetupBuildContainerConfig(config types.StackerConfig, storage types.Storage, c *container.Container, name string) error {
	rootfsPivot := path.Join(config.StackerDir, "rootfsPivot")
	if err := os.MkdirAll(rootfsPivot, 0755); err != nil {
		return err
	}

	if err := c.SetConfig("lxc.rootfs.mount", rootfsPivot); err != nil {
		return err
	}

	configs := map[string]string{
		"lxc.mount.auto":                "proc:mixed",
		"lxc.autodev":                   "1",
		"lxc.pty.max":                   "1024",
		"lxc.mount.entry":               "none dev/shm tmpfs defaults,create=dir 0 0",
		"lxc.uts.name":                  "stacker",
		"lxc.net.0.type":                "none",
		"lxc.environment":               fmt.Sprintf("PATH=%s", container.ReasonableDefaultPath),
		"lxc.apparmor.allow_incomplete": "1",
	}

	if err := c.SetConfigs(configs); err != nil {
		return err
	}

	err := c.BindMount("/sys", "/sys", "")
	if err != nil {
		return err
	}

	err = c.BindMount("/etc/resolv.conf", "/etc/resolv.conf", "")
	if err != nil {
		return err
	}

	rootfs, err := storage.GetLXCRootfsConfig(name)
	if err != nil {
		return err
	}

	err = c.SetConfig("lxc.rootfs.path", rootfs)
	if err != nil {
		return err
	}

	// liblxc inserts an apparmor profile if we don't set one by default.
	// however, since we may be statically linked with no packaging
	// support, the host may not have this default profile. let's check for
	// it. of course, we can't check for it by catting the value in
	// securityfs, because that's restricted :). so we fork and try to
	// change to the profile in question instead.
	//
	// note that this is not strictly correct: lxc will try to use a
	// non-cgns profile if cgns isn't supported by the kernel, but most
	// kernels these days support it so we ignore this case.
	lxcDefaultProfile := "lxc-container-default-cgns"
	err = runInternalGoSubcommand(config, []string{"check-aa-profile", lxcDefaultProfile})
	if err != nil {
		log.Infof("couldn't find AppArmor profile %s", lxcDefaultProfile)
		err = c.SetConfig("lxc.apparmor.profile", "unconfined")
		if err != nil {
			return err
		}
	}

	return nil
}

func SetupLayerConfig(config types.StackerConfig, c *container.Container, l types.Layer, name string) error {
	env, err := l.BuildEnvironment(name)
	if err != nil {
		return err
	}

	importsDir := path.Join(config.StackerDir, "imports", name)
	if _, err := os.Stat(importsDir); err == nil {
		log.Debugf("bind mounting %s into container", importsDir)
		err = c.BindMount(importsDir, "/stacker", "ro")
		if err != nil {
			return err
		}
	} else {
		log.Debugf("not bind mounting %s into container", importsDir)
	}

	for k, v := range env {
		if v != "" {
			err = c.SetConfig("lxc.environment", fmt.Sprintf("%s=%s", k, v))
			if err != nil {
				return err
			}
		}
	}

	for _, bind := range l.Binds {
		err = c.BindMount(bind.Source, bind.Dest, "")
		if err != nil {
			return err
		}
	}

	return err
}
