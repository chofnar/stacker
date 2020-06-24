package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"

	"github.com/anuvu/stacker/log"
	"github.com/anuvu/stacker/types"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/pkg/errors"
)

func ResolveIdmapSet() (*idmap.IdmapSet, error) {
	// TODO: we should try to use user namespaces when we're root as well.
	// For now we don't.
	if os.Geteuid() == 0 {
		log.Debugf("No uid mappings, running as root")
		return nil, nil
	}

	currentUser, err := user.Current()
	if err != nil {
		return nil, errors.Wrapf(err, "couldn't resolve current user")
	}

	idmapSet, err := idmap.DefaultIdmapSet("", currentUser.Username)
	if err != nil {
		return nil, errors.Wrapf(err, "failed parsing /etc/sub{u,g}idmap")
	}

	if idmapSet != nil {
		/* Let's make our current user the root user in the ns, so that when
		 * stacker emits files, it does them as the right user.
		 */
		hostMap := []idmap.IdmapEntry{
			idmap.IdmapEntry{
				Isuid:    true,
				Hostid:   int64(os.Getuid()),
				Nsid:     0,
				Maprange: 1,
			},
			idmap.IdmapEntry{
				Isgid:    true,
				Hostid:   int64(os.Getgid()),
				Nsid:     0,
				Maprange: 1,
			},
		}

		for _, hm := range hostMap {
			err := idmapSet.AddSafe(hm)
			if err != nil {
				return nil, errors.Wrapf(err, "failed adding idmap entry: %v", hm)
			}
		}
	}

	return idmapSet, nil
}

func runInUserns(idmapSet *idmap.IdmapSet, userCmd []string, msg string) error {
	if idmapSet == nil {
		return errors.Errorf("no subuids!")
	}

	args := []string{}
	for _, idm := range idmapSet.Idmap {
		var which string
		if idm.Isuid && idm.Isgid {
			which = "b"
		} else if idm.Isuid {
			which = "u"
		} else if idm.Isgid {
			which = "g"
		}

		m := fmt.Sprintf("%s:%d:%d:%d", which, idm.Nsid, idm.Hostid, idm.Maprange)
		args = append(args, "-m", m)
	}

	args = append(args, "--")
	args = append(args, userCmd...)

	cmd := exec.Command("lxc-usernsexec", args...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return errors.Wrapf(err, msg)
	}

	return nil
}

// A wrapper which runs things in a userns if we're an unprivileged user with
// an idmap, or runs things on the host if we're root and don't.
func MaybeRunInUserns(userCmd []string, msg string) error {
	idmapSet, err := ResolveIdmapSet()
	if err != nil {
		return err
	}

	if idmapSet == nil {
		if os.Geteuid() != 0 {
			return errors.Errorf("no idmap and not root, can't run %v", userCmd)
		}

		cmd := exec.Command(userCmd[0], userCmd[1:]...)
		cmd.Stdin = nil
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return errors.Wrapf(cmd.Run(), msg)
	}

	return runInUserns(idmapSet, userCmd, msg)
}

func RunUmociSubcommand(config types.StackerConfig, args []string) error {
	binary, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return err
	}

	cmd := []string{
		binary,
		"--oci-dir", config.OCIDir,
		"--roots-dir", config.RootFSDir,
		"--stacker-dir", config.StackerDir,
		"--storage-type", config.StorageType,
	}

	if config.Debug {
		cmd = append(cmd, "--debug")
	}

	cmd = append(cmd, "umoci")
	cmd = append(cmd, args...)
	return MaybeRunInUserns(cmd, "image unpack failed")
}
