package generate

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containers/common/pkg/config"
	"github.com/containers/libpod/libpod"
	"github.com/containers/libpod/libpod/image"
	"github.com/containers/libpod/pkg/specgen"
	"github.com/containers/libpod/pkg/util"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// TODO unify this in one place - maybe libpod/define
const (
	// TypeBind is the type for mounting host dir
	TypeBind = "bind"
	// TypeVolume is the type for named volumes
	TypeVolume = "volume"
	// TypeTmpfs is the type for mounting tmpfs
	TypeTmpfs = "tmpfs"
)

var (
	errDuplicateDest = errors.Errorf("duplicate mount destination")
)

// Produce final mounts and named volumes for a container
func finalizeMounts(ctx context.Context, s *specgen.SpecGenerator, rt *libpod.Runtime, rtc *config.Config, img *image.Image) ([]spec.Mount, []*specgen.NamedVolume, error) {
	// Get image volumes
	baseMounts, baseVolumes, err := getImageVolumes(ctx, img, s)
	if err != nil {
		return nil, nil, err
	}

	// Get volumes-from mounts
	volFromMounts, volFromVolumes, err := getVolumesFrom(s.VolumesFrom, rt)
	if err != nil {
		return nil, nil, err
	}

	// Supercede from --volumes-from.
	for dest, mount := range volFromMounts {
		baseMounts[dest] = mount
	}
	for dest, volume := range volFromVolumes {
		baseVolumes[dest] = volume
	}

	// Need to make map forms of specgen mounts/volumes.
	unifiedMounts := map[string]spec.Mount{}
	unifiedVolumes := map[string]*specgen.NamedVolume{}
	for _, m := range s.Mounts {
		if _, ok := unifiedMounts[m.Destination]; ok {
			return nil, nil, errors.Wrapf(errDuplicateDest, "conflict in specified mounts - multiple mounts at %q", m.Destination)
		}
		unifiedMounts[m.Destination] = m
	}
	for _, v := range s.Volumes {
		if _, ok := unifiedVolumes[v.Dest]; ok {
			return nil, nil, errors.Wrapf(errDuplicateDest, "conflict in specified volumes - multiple volumes at %q", v.Dest)
		}
		unifiedVolumes[v.Dest] = v
	}

	// If requested, add container init binary
	if s.Init {
		initPath := s.InitPath
		if initPath == "" && rtc != nil {
			initPath = rtc.Engine.InitPath
		}
		initMount, err := addContainerInitBinary(s, initPath)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := unifiedMounts[initMount.Destination]; ok {
			return nil, nil, errors.Wrapf(errDuplicateDest, "conflict with mount added by --init to %q", initMount.Destination)
		}
		unifiedMounts[initMount.Destination] = initMount
	}

	// Before superseding, we need to find volume mounts which conflict with
	// named volumes, and vice versa.
	// We'll delete the conflicts here as we supersede.
	for dest := range unifiedMounts {
		if _, ok := baseVolumes[dest]; ok {
			delete(baseVolumes, dest)
		}
	}
	for dest := range unifiedVolumes {
		if _, ok := baseMounts[dest]; ok {
			delete(baseMounts, dest)
		}
	}

	// Supersede volumes-from/image volumes with unified volumes from above.
	// This is an unconditional replacement.
	for dest, mount := range unifiedMounts {
		baseMounts[dest] = mount
	}
	for dest, volume := range unifiedVolumes {
		baseVolumes[dest] = volume
	}

	// TODO: Investigate moving readonlyTmpfs into here. Would be more
	// correct.

	// Check for conflicts between named volumes and mounts
	for dest := range baseMounts {
		if _, ok := baseVolumes[dest]; ok {
			return nil, nil, errors.Wrapf(errDuplicateDest, "conflict at mount destination %v", dest)
		}
	}
	for dest := range baseVolumes {
		if _, ok := baseMounts[dest]; ok {
			return nil, nil, errors.Wrapf(errDuplicateDest, "conflict at mount destination %v", dest)
		}
	}
	// Final step: maps to arrays
	finalMounts := make([]spec.Mount, 0, len(baseMounts))
	for _, mount := range baseMounts {
		if mount.Type == TypeBind {
			absSrc, err := filepath.Abs(mount.Source)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "error getting absolute path of %s", mount.Source)
			}
			mount.Source = absSrc
		}
		finalMounts = append(finalMounts, mount)
	}
	finalVolumes := make([]*specgen.NamedVolume, 0, len(baseVolumes))
	for _, volume := range baseVolumes {
		finalVolumes = append(finalVolumes, volume)
	}

	return finalMounts, finalVolumes, nil
}

// Get image volumes from the given image
func getImageVolumes(ctx context.Context, img *image.Image, s *specgen.SpecGenerator) (map[string]spec.Mount, map[string]*specgen.NamedVolume, error) {
	mounts := make(map[string]spec.Mount)
	volumes := make(map[string]*specgen.NamedVolume)

	mode := strings.ToLower(s.ImageVolumeMode)

	// Image may be nil (rootfs in use), or image volume mode may be ignore.
	if img == nil || mode == "ignore" {
		return mounts, volumes, nil
	}

	inspect, err := img.InspectNoSize(ctx)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error inspecting image to get image volumes")
	}
	for volume := range inspect.Config.Volumes {
		logrus.Debugf("Image has volume at %q", volume)
		cleanDest := filepath.Clean(volume)
		switch mode {
		case "", "anonymous":
			// Anonymous volumes have no name.
			newVol := new(specgen.NamedVolume)
			newVol.Dest = cleanDest
			newVol.Options = []string{"rprivate", "rw", "nodev", "exec"}
			volumes[cleanDest] = newVol
			logrus.Debugf("Adding anonymous image volume at %q", cleanDest)
		case "tmpfs":
			mount := spec.Mount{
				Destination: cleanDest,
				Source:      TypeTmpfs,
				Type:        TypeTmpfs,
				Options:     []string{"rprivate", "rw", "nodev", "exec"},
			}
			mounts[cleanDest] = mount
			logrus.Debugf("Adding tmpfs image volume at %q", cleanDest)
		}
	}

	return mounts, volumes, nil
}

func getVolumesFrom(volumesFrom []string, runtime *libpod.Runtime) (map[string]spec.Mount, map[string]*specgen.NamedVolume, error) {
	finalMounts := make(map[string]spec.Mount)
	finalNamedVolumes := make(map[string]*specgen.NamedVolume)

	for _, volume := range volumesFrom {
		var options []string

		splitVol := strings.SplitN(volume, ":", 2)
		if len(splitVol) == 2 {
			splitOpts := strings.Split(splitVol[1], ",")
			for _, opt := range splitOpts {
				setRORW := false
				setZ := false
				switch opt {
				case "z":
					if setZ {
						return nil, nil, errors.Errorf("cannot set :z more than once in mount options")
					}
					setZ = true
				case "ro", "rw":
					if setRORW {
						return nil, nil, errors.Errorf("cannot set ro or rw options more than once")
					}
					setRORW = true
				default:
					return nil, nil, errors.Errorf("invalid option %q specified - volumes from another container can only use z,ro,rw options", opt)
				}
			}
			options = splitOpts
		}

		ctr, err := runtime.LookupContainer(splitVol[0])
		if err != nil {
			return nil, nil, errors.Wrapf(err, "error looking up container %q for volumes-from", splitVol[0])
		}

		logrus.Debugf("Adding volumes from container %s", ctr.ID())

		// Look up the container's user volumes. This gets us the
		// destinations of all mounts the user added to the container.
		userVolumesArr := ctr.UserVolumes()

		// We're going to need to access them a lot, so convert to a map
		// to reduce looping.
		// We'll also use the map to indicate if we missed any volumes along the way.
		userVolumes := make(map[string]bool)
		for _, dest := range userVolumesArr {
			userVolumes[dest] = false
		}

		// Now we get the container's spec and loop through its volumes
		// and append them in if we can find them.
		spec := ctr.Spec()
		if spec == nil {
			return nil, nil, errors.Errorf("error retrieving container %s spec for volumes-from", ctr.ID())
		}
		for _, mnt := range spec.Mounts {
			if mnt.Type != TypeBind {
				continue
			}
			if _, exists := userVolumes[mnt.Destination]; exists {
				userVolumes[mnt.Destination] = true

				if len(options) != 0 {
					mnt.Options = options
				}

				if _, ok := finalMounts[mnt.Destination]; ok {
					logrus.Debugf("Overriding mount to %s with new mount from container %s", mnt.Destination, ctr.ID())
				}
				finalMounts[mnt.Destination] = mnt
			}
		}

		// We're done with the spec mounts. Add named volumes.
		// Add these unconditionally - none of them are automatically
		// part of the container, as some spec mounts are.
		namedVolumes := ctr.NamedVolumes()
		for _, namedVol := range namedVolumes {
			if _, exists := userVolumes[namedVol.Dest]; exists {
				userVolumes[namedVol.Dest] = true
			}

			if len(options) != 0 {
				namedVol.Options = options
			}

			if _, ok := finalMounts[namedVol.Dest]; ok {
				logrus.Debugf("Overriding named volume mount to %s with new named volume from container %s", namedVol.Dest, ctr.ID())
			}

			newVol := new(specgen.NamedVolume)
			newVol.Dest = namedVol.Dest
			newVol.Options = namedVol.Options
			newVol.Name = namedVol.Name

			finalNamedVolumes[namedVol.Dest] = newVol
		}

		// Check if we missed any volumes
		for volDest, found := range userVolumes {
			if !found {
				logrus.Warnf("Unable to match volume %s from container %s for volumes-from", volDest, ctr.ID())
			}
		}
	}

	return finalMounts, finalNamedVolumes, nil
}

// AddContainerInitBinary adds the init binary specified by path iff the
// container will run in a private PID namespace that is not shared with the
// host or another pre-existing container, where an init-like process is
// already running.
// This does *NOT* modify the container command - that must be done elsewhere.
func addContainerInitBinary(s *specgen.SpecGenerator, path string) (spec.Mount, error) {
	mount := spec.Mount{
		Destination: "/dev/init",
		Type:        TypeBind,
		Source:      path,
		Options:     []string{TypeBind, "ro"},
	}

	if path == "" {
		return mount, fmt.Errorf("please specify a path to the container-init binary")
	}
	if !s.PidNS.IsPrivate() {
		return mount, fmt.Errorf("cannot add init binary as PID 1 (PID namespace isn't private)")
	}
	if s.Systemd == "true" || s.Systemd == "always" {
		return mount, fmt.Errorf("cannot use container-init binary with systemd")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return mount, errors.Wrap(err, "container-init binary not found on the host")
	}
	return mount, nil
}

// Supersede existing mounts in the spec with new, user-specified mounts.
// TODO: Should we unmount subtree mounts? E.g., if /tmp/ is mounted by
// one mount, and we already have /tmp/a and /tmp/b, should we remove
// the /tmp/a and /tmp/b mounts in favor of the more general /tmp?
func SupercedeUserMounts(mounts []spec.Mount, configMount []spec.Mount) []spec.Mount {
	if len(mounts) > 0 {
		// If we have overlappings mounts, remove them from the spec in favor of
		// the user-added volume mounts
		destinations := make(map[string]bool)
		for _, mount := range mounts {
			destinations[path.Clean(mount.Destination)] = true
		}
		// Copy all mounts from spec to defaultMounts, except for
		//  - mounts overridden by a user supplied mount;
		//  - all mounts under /dev if a user supplied /dev is present;
		mountDev := destinations["/dev"]
		for _, mount := range configMount {
			if _, ok := destinations[path.Clean(mount.Destination)]; !ok {
				if mountDev && strings.HasPrefix(mount.Destination, "/dev/") {
					// filter out everything under /dev if /dev is user-mounted
					continue
				}

				logrus.Debugf("Adding mount %s", mount.Destination)
				mounts = append(mounts, mount)
			}
		}
		return mounts
	}
	return configMount
}

func InitFSMounts(mounts []spec.Mount) error {
	for i, m := range mounts {
		switch {
		case m.Type == TypeBind:
			opts, err := util.ProcessOptions(m.Options, false, m.Source)
			if err != nil {
				return err
			}
			mounts[i].Options = opts
		case m.Type == TypeTmpfs && filepath.Clean(m.Destination) != "/dev":
			opts, err := util.ProcessOptions(m.Options, true, "")
			if err != nil {
				return err
			}
			mounts[i].Options = opts
		}
	}
	return nil
}